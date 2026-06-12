// sources.go implements the Phase 1 read-plane endpoints: managing state-source
// connections and reading/analyzing the state they contain.
package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/terraform-state-manager/terraform-state-manager/internal/analyzer"
	"github.com/terraform-state-manager/terraform-state-manager/internal/crypto"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
	"github.com/terraform-state-manager/terraform-state-manager/internal/reporting"
	"github.com/terraform-state-manager/terraform-state-manager/internal/services/statesync"
	"github.com/terraform-state-manager/terraform-state-manager/internal/statesource"
)

// SourcesHandlers serves state-source, analysis, edit, and transfer endpoints.
type SourcesHandlers struct {
	repo         *repositories.SourceRepository
	editRepo     *repositories.StateEditRepository
	transferRepo *repositories.TransferRepository
	lockRepo     *repositories.StateLockRepository
	analysisRepo *repositories.StateAnalysisRepository
	audit        auditor
	// syncer reconciles the persistent analysis store; nil in rigs that don't
	// exercise the dashboard (handlers must nil-check).
	syncer *statesync.Syncer
}

// NewSourcesHandlers constructs the handlers over the app (public) connection,
// where the state_sources, state_backups, state_edits, and state_transfers
// tables live. identityDB (may be nil) carries the shared audit log.
func NewSourcesHandlers(database, identityDB *sql.DB) *SourcesHandlers {
	return &SourcesHandlers{
		repo:         repositories.NewSourceRepository(database),
		editRepo:     repositories.NewStateEditRepository(database),
		transferRepo: repositories.NewTransferRepository(database),
		lockRepo:     repositories.NewStateLockRepository(database),
		analysisRepo: repositories.NewStateAnalysisRepository(database),
		audit:        newAuditor(identityDB),
	}
}

// AttachSyncer wires the statesync service in after construction (the router
// builds both and connects them).
func (h *SourcesHandlers) AttachSyncer(s *statesync.Syncer) { h.syncer = s }

// ConnectSource builds a live connector for a source, decrypting its
// credentials. Exported shape for the statesync service's Connect dependency.
func ConnectSource(s *repositories.Source) (statesource.Connector, error) {
	creds, err := decryptCredentials(s)
	if err != nil {
		return nil, err
	}
	return statesource.New(s.Type, s.Config, creds)
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
		h.audit.write(c, "source.create", "source", created.ID,
			map[string]interface{}{"name": created.Name, "type": created.Type})
		// Backfill the new source's analyses right away rather than waiting for
		// the next cycle (no-op if a cycle is already running).
		if h.syncer != nil {
			go func() { _ = h.syncer.SyncAll(context.Background()) }()
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

// UpdateSource edits a source's name, config, scope, and (optionally)
// credentials. Type is immutable; blank credentials keep the stored secret.
// @Summary      Update state source
// @Description  Edits a state-source connection. Credentials are replaced only when provided; the type cannot change. Requires sources:manage.
// @Tags         Sources
// @Accept       json
// @Produce      json
// @Param        id   path      string  true  "Source ID"
// @Success      200  {object}  map[string]interface{}
// @Failure      400  {object}  map[string]interface{}
// @Failure      404  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     CookieAuth
// @Router       /sources/{id} [put]
func (h *SourcesHandlers) UpdateSource() gin.HandlerFunc {
	return func(c *gin.Context) {
		existing, err := h.repo.GetByID(c.Request.Context(), c.Param("id"))
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load source"})
			return
		}
		if existing == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "source not found"})
			return
		}

		var req struct {
			Name        string         `json:"name" binding:"required"`
			Endpoint    string         `json:"endpoint"`
			Config      map[string]any `json:"config"`
			Scope       map[string]any `json:"scope"`
			Credentials map[string]any `json:"credentials"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "name is required"})
			return
		}

		// Validate the connector against the new config, using the new
		// credentials when given, otherwise the stored ones.
		validateCreds := req.Credentials
		if len(validateCreds) == 0 {
			stored, dErr := decryptCredentials(existing)
			if dErr != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to decrypt source credentials"})
				return
			}
			validateCreds = stored
		}
		if _, err := statesource.New(existing.Type, req.Config, validateCreds); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		src := &repositories.Source{
			ID:       existing.ID,
			Name:     req.Name,
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

		updated, err := h.repo.Update(c.Request.Context(), src)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update source"})
			return
		}
		if updated == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "source not found"})
			return
		}
		h.audit.write(c, "source.update", "source", updated.ID,
			map[string]interface{}{"name": updated.Name, "type": updated.Type})
		// The new config may point at different states; reconcile soon.
		if h.syncer != nil {
			go func() { _ = h.syncer.SyncAll(context.Background()) }()
		}
		c.JSON(http.StatusOK, updated)
	}
}

// TestSource connects to the source's backend and lists its states, returning
// the count or the connection error. Read-only; nothing is persisted.
// @Summary      Test state source connection
// @Description  Connects to the backend and lists states. Requires state:read.
// @Tags         Sources
// @Produce      json
// @Param        id   path      string  true  "Source ID"
// @Success      200  {object}  map[string]interface{}
// @Failure      502  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     CookieAuth
// @Router       /sources/{id}/test [post]
func (h *SourcesHandlers) TestSource() gin.HandlerFunc {
	return func(c *gin.Context) {
		conn, ok := h.connectorFor(c)
		if !ok {
			return
		}
		ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
		defer cancel()
		refs, err := conn.List(ctx)
		if err != nil {
			c.JSON(http.StatusBadGateway, gin.H{"status": "failed", "error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "ok", "states": len(refs)})
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
		id := c.Param("id")
		if err := h.repo.Delete(c.Request.Context(), id); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete source"})
			return
		}
		h.audit.write(c, "source.delete", "source", id, nil)
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
		// Some backends list no byte size (HCP workspaces); overlay the sizes
		// the analysis store recorded when it last read each state. Best
		// effort — a store miss leaves the connector's value.
		if sizes, err := h.analysisRepo.Sizes(c.Request.Context(), c.Param("id")); err == nil {
			for i := range refs {
				if refs[i].Size == 0 {
					if sz, ok := sizes[refs[i].Key]; ok {
						refs[i].Size = sz
					}
				}
			}
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

// StateOutputs returns the root-module outputs for a state file (?key=...).
// Sensitive output values are redacted server-side.
// @Summary      List state outputs
// @Tags         Sources
// @Produce      json
// @Param        id   path  string  true  "Source ID"
// @Param        key  query string  true  "State file key"
// @Success      200  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     CookieAuth
// @Router       /sources/{id}/state/outputs [get]
func (h *SourcesHandlers) StateOutputs() gin.HandlerFunc {
	return func(c *gin.Context) {
		rs, ok := h.readState(c)
		if !ok {
			return
		}
		outputs, err := analyzer.ListOutputs(rs.Data)
		if err != nil {
			c.JSON(http.StatusUnprocessableEntity, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"key": rs.Key, "outputs": outputs})
	}
}

// StateHistory returns a state's analysis snapshots (newest first) from the
// append-only history the statesync service maintains — one row per observed
// change, powering per-state time series and point-in-time comparison.
// @Summary      State analysis history
// @Tags         Sources
// @Produce      json
// @Param        id     path   string  true   "Source ID"
// @Param        key    query  string  true   "State file key"
// @Param        limit  query  int     false  "Max snapshots (default 200)"
// @Success      200  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     CookieAuth
// @Router       /sources/{id}/state/history [get]
func (h *SourcesHandlers) StateHistory() gin.HandlerFunc {
	return func(c *gin.Context) {
		key := c.Query("key")
		if key == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "key query parameter is required"})
			return
		}
		limit, _ := strconv.Atoi(c.DefaultQuery("limit", "0"))
		history, err := h.analysisRepo.History(c.Request.Context(), c.Param("id"), key, limit)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load analysis history"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"key": key, "history": history})
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
		h.audit.write(c, "report.generate", "state", c.Param("id"),
			map[string]interface{}{"key": rs.Key, "format": string(format)})
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
