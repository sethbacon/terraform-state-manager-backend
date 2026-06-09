// sources.go implements the Phase 1 read-plane endpoints: managing state-source
// connections and reading/analyzing the state they contain.
package api

import (
	"database/sql"
	"encoding/json"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/terraform-state-manager/terraform-state-manager/internal/analyzer"
	"github.com/terraform-state-manager/terraform-state-manager/internal/crypto"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
	"github.com/terraform-state-manager/terraform-state-manager/internal/reporting"
	"github.com/terraform-state-manager/terraform-state-manager/internal/statesource"
)

// SourcesHandlers serves state-source, analysis, edit, and transfer endpoints.
type SourcesHandlers struct {
	repo         *repositories.SourceRepository
	editRepo     *repositories.StateEditRepository
	transferRepo *repositories.TransferRepository
	lockRepo     *repositories.StateLockRepository
}

// NewSourcesHandlers constructs the handlers over the app (public) connection,
// where the state_sources, state_backups, state_edits, and state_transfers
// tables live.
func NewSourcesHandlers(database *sql.DB) *SourcesHandlers {
	return &SourcesHandlers{
		repo:         repositories.NewSourceRepository(database),
		editRepo:     repositories.NewStateEditRepository(database),
		transferRepo: repositories.NewTransferRepository(database),
		lockRepo:     repositories.NewStateLockRepository(database),
	}
}

// ListSources returns all configured state sources.
// @Summary      List state sources
// @Description  Returns all configured state-source connections (secrets are never included). Requires state:read.
// @Tags         Sources
// @Produce      json
// @Success      200  {object}  map[string]interface{}
// @Failure      401  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     CookieAuth
// @Router       /sources [get]
func (h *SourcesHandlers) ListSources() gin.HandlerFunc {
	return func(c *gin.Context) {
		sources, err := h.repo.List(c.Request.Context())
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list sources"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"sources": sources})
	}
}

// CreateSource adds a state source after validating its connector config.
// @Summary      Create state source
// @Description  Adds a state-source connection (credentials encrypted at rest). Requires sources:manage.
// @Tags         Sources
// @Accept       json
// @Produce      json
// @Success      201  {object}  map[string]interface{}
// @Failure      400  {object}  map[string]interface{}
// @Failure      401  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     CookieAuth
// @Router       /sources [post]
func (h *SourcesHandlers) CreateSource() gin.HandlerFunc {
	return func(c *gin.Context) {
		var req struct {
			Name        string         `json:"name" binding:"required"`
			Type        string         `json:"type" binding:"required"`
			Endpoint    string         `json:"endpoint"`
			Config      map[string]any `json:"config"`
			Scope       map[string]any `json:"scope"`
			Credentials map[string]any `json:"credentials"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "name and type are required"})
			return
		}
		// Validate the connector up front (e.g. local base_path exists, HCP token present).
		if _, err := statesource.New(req.Type, req.Config, req.Credentials); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		src := &repositories.Source{
			Name:     req.Name,
			Type:     req.Type,
			Endpoint: req.Endpoint,
			Config:   req.Config,
			Scope:    req.Scope,
		}
		if len(req.Credentials) > 0 {
			if !crypto.Available() {
				c.JSON(http.StatusBadRequest, gin.H{
					"error": "cannot store credentials: encryption key not configured (set TSM_ENCRYPTION_KEY)",
				})
				return
			}
			plain, _ := json.Marshal(req.Credentials)
			enc, err := crypto.Encrypt(plain)
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to encrypt credentials"})
				return
			}
			src.EncryptedCredentials = enc
		}

		created, err := h.repo.Create(c.Request.Context(), src)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create source"})
			return
		}
		c.JSON(http.StatusCreated, created)
	}
}

// GetSource returns a single source.
// @Summary      Get state source
// @Tags         Sources
// @Produce      json
// @Param        id   path      string  true  "Source ID"
// @Success      200  {object}  map[string]interface{}
// @Failure      404  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     CookieAuth
// @Router       /sources/{id} [get]
func (h *SourcesHandlers) GetSource() gin.HandlerFunc {
	return func(c *gin.Context) {
		s, err := h.repo.GetByID(c.Request.Context(), c.Param("id"))
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load source"})
			return
		}
		if s == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "source not found"})
			return
		}
		c.JSON(http.StatusOK, s)
	}
}

// DeleteSource removes a source.
// @Summary      Delete state source
// @Description  Disconnects the source from the State Manager. The underlying backend and its files are not touched. Requires sources:manage.
// @Tags         Sources
// @Param        id   path  string  true  "Source ID"
// @Success      204  "No Content"
// @Security     BearerAuth
// @Security     CookieAuth
// @Router       /sources/{id} [delete]
func (h *SourcesHandlers) DeleteSource() gin.HandlerFunc {
	return func(c *gin.Context) {
		if err := h.repo.Delete(c.Request.Context(), c.Param("id")); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete source"})
			return
		}
		c.Status(http.StatusNoContent)
	}
}

// ListStates enumerates the state files available under a source.
// @Summary      List state files
// @Tags         Sources
// @Produce      json
// @Param        id   path      string  true  "Source ID"
// @Success      200  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     CookieAuth
// @Router       /sources/{id}/states [get]
func (h *SourcesHandlers) ListStates() gin.HandlerFunc {
	return func(c *gin.Context) {
		conn, ok := h.connectorFor(c)
		if !ok {
			return
		}
		refs, err := conn.List(c.Request.Context())
		if err != nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"states": refs})
	}
}

// AnalyzeState returns the analyzer metrics for a single state file (?key=...).
// @Summary      Analyze state file
// @Description  RUM, resource-type/provider/module breakdowns, and version info for a state file.
// @Tags         Sources
// @Produce      json
// @Param        id   path      string  true  "Source ID"
// @Param        key  query     string  true  "State file key"
// @Success      200  {object}  map[string]interface{}
// @Failure      422  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     CookieAuth
// @Router       /sources/{id}/state/analysis [get]
func (h *SourcesHandlers) AnalyzeState() gin.HandlerFunc {
	return func(c *gin.Context) {
		rs, ok := h.readState(c)
		if !ok {
			return
		}
		a, err := analyzer.Analyze(rs.Data)
		if err != nil {
			c.JSON(http.StatusUnprocessableEntity, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"key":           rs.Key,
			"size":          rs.Size,
			"last_modified": rs.LastModified,
			"analysis":      a,
		})
	}
}

// RawState streams the raw state JSON (?key=...).
// @Summary      Get raw state JSON
// @Tags         Sources
// @Produce      json
// @Param        id   path  string  true  "Source ID"
// @Param        key  query string  true  "State file key"
// @Success      200  {string}  string  "raw state JSON"
// @Security     BearerAuth
// @Security     CookieAuth
// @Router       /sources/{id}/state/raw [get]
func (h *SourcesHandlers) RawState() gin.HandlerFunc {
	return func(c *gin.Context) {
		rs, ok := h.readState(c)
		if !ok {
			return
		}
		c.Data(http.StatusOK, "application/json", rs.Data)
	}
}

// ListStateResources returns the per-resource summary for a state file (?key=...).
// @Summary      List state resources
// @Tags         Sources
// @Produce      json
// @Param        id   path  string  true  "Source ID"
// @Param        key  query string  true  "State file key"
// @Success      200  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     CookieAuth
// @Router       /sources/{id}/state/resources [get]
func (h *SourcesHandlers) ListStateResources() gin.HandlerFunc {
	return func(c *gin.Context) {
		rs, ok := h.readState(c)
		if !ok {
			return
		}
		res, err := analyzer.ListResources(rs.Data)
		if err != nil {
			c.JSON(http.StatusUnprocessableEntity, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"key": rs.Key, "resources": res})
	}
}

// StateReport renders the analysis as a downloadable report (?key=...&format=json|md|csv).
// @Summary      Download analysis report
// @Tags         Sources
// @Produce      json,text/markdown,text/csv
// @Param        id      path   string  true   "Source ID"
// @Param        key     query  string  true   "State file key"
// @Param        format  query  string  false  "Report format: json, md, or csv"  Enums(json, md, csv)
// @Success      200  {string}  string  "report file"
// @Security     BearerAuth
// @Security     CookieAuth
// @Router       /sources/{id}/state/report [get]
func (h *SourcesHandlers) StateReport() gin.HandlerFunc {
	return func(c *gin.Context) {
		rs, ok := h.readState(c)
		if !ok {
			return
		}
		a, err := analyzer.Analyze(rs.Data)
		if err != nil {
			c.JSON(http.StatusUnprocessableEntity, gin.H{"error": err.Error()})
			return
		}
		format := reporting.Format(c.DefaultQuery("format", "json"))
		contentType, filename, body, err := reporting.Generate(a, rs.Key, format)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.Header("Content-Disposition", `attachment; filename="`+filename+`"`)
		c.Data(http.StatusOK, contentType, body)
	}
}

// connectorFor loads the source by :id and builds its connector, writing an error
// response and returning ok=false on failure.
func (h *SourcesHandlers) connectorFor(c *gin.Context) (statesource.Connector, bool) {
	s, err := h.repo.GetByID(c.Request.Context(), c.Param("id"))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load source"})
		return nil, false
	}
	if s == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "source not found"})
		return nil, false
	}
	creds, err := decryptCredentials(s)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to decrypt source credentials"})
		return nil, false
	}
	conn, err := statesource.New(s.Type, s.Config, creds)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return nil, false
	}
	return conn, true
}

// decryptCredentials returns the source's decrypted credentials map, or nil when
// the source stores none.
func decryptCredentials(s *repositories.Source) (map[string]any, error) {
	if len(s.EncryptedCredentials) == 0 {
		return nil, nil
	}
	plain, err := crypto.Decrypt(s.EncryptedCredentials)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(plain, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// readState resolves the connector and reads the state identified by ?key=.
func (h *SourcesHandlers) readState(c *gin.Context) (*statesource.RawState, bool) {
	key := c.Query("key")
	if key == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "key query parameter is required"})
		return nil, false
	}
	conn, ok := h.connectorFor(c)
	if !ok {
		return nil, false
	}
	rs, err := conn.Read(c.Request.Context(), key)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return nil, false
	}
	return rs, true
}
