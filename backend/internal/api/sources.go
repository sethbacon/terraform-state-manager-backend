// sources.go implements the Phase 1 read-plane endpoints: managing state-source
// connections and reading/analyzing the state they contain.
package api

import (
	"errors"

	"context"
	"database/sql"
	"encoding/json"
	"github.com/terraform-state-manager/terraform-state-manager/internal/tenantscope"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/sethbacon/terraform-suite-identity/identity/suite"
	"github.com/terraform-state-manager/terraform-state-manager/internal/analyzer"
	"github.com/terraform-state-manager/terraform-state-manager/internal/crypto"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"

	"github.com/terraform-state-manager/terraform-state-manager/internal/reporting"
	"github.com/terraform-state-manager/terraform-state-manager/internal/services/statesync"
	"github.com/terraform-state-manager/terraform-state-manager/internal/statesource"
)

// SourcesHandlers serves state-source, analysis, edit, and transfer endpoints.
type SourcesHandlers struct {
	repo          *repositories.SourceRepository
	editRepo      *repositories.StateEditRepository
	transferRepo  *repositories.TransferRepository
	lockRepo      *repositories.StateLockRepository
	analysisRepo  *repositories.StateAnalysisRepository
	moduleRefRepo *repositories.StateModuleRefRepository
	audit         auditor
	// syncer reconciles the persistent analysis store; nil in rigs that don't
	// exercise the dashboard (handlers must nil-check).
	syncer *statesync.Syncer
	// overviewCache memoizes the dashboard's store-wide aggregations (see
	// dashboard.go). Zero value ready; guarded by its own mutex.
	overviewCache overviewAggCache
	// tenantDualRead runs the organization-scoped read beside the unscoped read
	// on the two /sources read routes and reports where they disagree, without
	// changing what is served (#393 Phase 2b, TSM_TENANCY_DUAL_READ). Off unless
	// the router turns it on; deleted in Phase 3 along with tenant_dualread.go.
	tenantDualRead bool
	// orgs verifies that an acting organization exists before a row is stamped
	// with it. Nil in rigs with no identity connection; actingOrganization
	// REFUSES rather than proceeding, because stamping an unverified id is the
	// orphaned-row case (#436).
	orgs organizationExistence
}

// NewSourcesHandlers constructs the handlers over the app (public) connection,
// where the state_sources, state_backups, state_edits, and state_transfers
// tables live. identityDB (may be nil) carries the shared audit log.
func NewSourcesHandlers(database, identityDB *sql.DB) *SourcesHandlers {
	return &SourcesHandlers{
		repo:          repositories.NewSourceRepository(database),
		editRepo:      repositories.NewStateEditRepository(database),
		transferRepo:  repositories.NewTransferRepository(database),
		lockRepo:      repositories.NewStateLockRepository(database),
		analysisRepo:  repositories.NewStateAnalysisRepository(database),
		moduleRefRepo: repositories.NewStateModuleRefRepository(database),
		audit:         newAuditor(identityDB),
	}
}

// AttachOrganizations wires the existence check used before a row is stamped
// with an acting organization (#436).
//
// A setter, and the router supplies its EXISTING approles.Members rather than
// this package constructing an identity repository of its own. That is not
// style: internal/approles' own guard test refuses a second construction of
// idstore.NewOrganizationRepository anywhere else, because a repository obtained
// that way writes identity WITHOUT mirroring to this application's
// organization_member_roles — and nothing observable would report it. The read
// here needs no mirroring, but honouring one construction point is what keeps
// that guard meaningful rather than something with an exception in it.
func (h *SourcesHandlers) AttachOrganizations(orgs organizationExistence) { h.orgs = orgs }

// AttachSyncer wires the statesync service in after construction (the router
// builds both and connects them).
func (h *SourcesHandlers) AttachSyncer(s *statesync.Syncer) { h.syncer = s }

// EnableTenantDualRead turns on the #393 Phase 2b equivalence measurement (see
// tenant_dualread.go). A setter rather than a constructor parameter for the
// reason AttachSyncer is one: the router holds the config and the handlers do
// not, and this is one line for Phase 3 to delete rather than a signature every
// caller and test would have to be walked through twice.
func (h *SourcesHandlers) EnableTenantDualRead(on bool) { h.tenantDualRead = on }

// ConnectSource builds a live connector for a source, decrypting its
// credentials. Exported shape for the statesync service's Connect dependency.
func ConnectSource(s *repositories.Source) (statesource.Connector, error) {
	creds, err := decryptCredentials(s)
	if err != nil {
		return nil, err
	}
	return statesource.New(s.Type, s.Config, creds)
}

// sourcePageSize is both the default and the maximum number of sources returned
// by ListSources. It bounds the response (#282) while sitting far above any
// realistic install — state_sources is operator-provisioned, so no existing
// client loses rows by gaining this cap. A client that wants smaller pages can
// ask for them; it cannot ask for a bigger one.
const sourcePageSize = 500

// ListSources returns the configured state sources, newest first.
// @Summary      List state sources
// @Description  Returns configured state-source connections (secrets are never included), newest first. Bounded to 500 per response; use ?page/?per_page to walk larger fleets and compare `total` against the returned count. Requires state:read.
// @Tags         Sources
// @Produce      json
// @Param        page      query  int  false  "Page number, 1-indexed (default 1)"
// @Param        per_page  query  int  false  "Items per page (max 500, default 500)"
// @Success      200  {object}  map[string]interface{}
// @Failure      401  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     CookieAuth
// @Router       /sources [get]
func (h *SourcesHandlers) ListSources() gin.HandlerFunc {
	return func(c *gin.Context) {
		perPage := sourcePageSize
		if v, err := strconv.Atoi(c.Query("per_page")); err == nil && v > 0 && v <= sourcePageSize {
			perPage = v
		}
		page := 1
		// Bound page so (page-1)*perPage cannot overflow int on a crafted value.
		if v, err := strconv.Atoi(c.Query("page")); err == nil && v > 0 && v <= 1_000_000 {
			page = v
		}
		ctx := c.Request.Context()
		sources, err := h.repo.ListPage(ctx, perPage, (page-1)*perPage)
		if err != nil {
			serverError(c, err, "failed to list sources")
			return
		}
		// total lets a client detect that the page it got is not the whole
		// fleet; without it the cap would truncate silently. The legacy
		// `sources` key is unchanged, so existing clients are unaffected.
		total, err := h.repo.Count(ctx)
		if err != nil {
			serverError(c, err, "failed to list sources")
			return
		}
		c.JSON(http.StatusOK, gin.H{"sources": sources, "total": total, "page": page, "per_page": perPage})

		// After the response, never before it: the measurement must not be able
		// to delay or replace a read that has already succeeded (#393 Phase 2b).
		if h.tenantDualRead {
			h.observeSourceListScope(c)
		}
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
				serverError(c, err, "failed to encrypt credentials")
				return
			}
			src.EncryptedCredentials = enc
		}

		// The organization this source belongs to, named by the request and
		// verified against a scope this server resolved (#436). Writes the
		// response itself on every failure path.
		orgID := actingOrganization(c, h.orgs)
		if orgID == "" {
			return
		}

		created, err := h.repo.Create(c.Request.Context(), src, orgID)
		if err != nil {
			serverError(c, err, "failed to create source")
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
			serverError(c, err, "failed to load source")
			return
		}
		if s == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "source not found"})
			return
		}
		c.JSON(http.StatusOK, s)

		// This is the read #393 names as the highest blast radius: /sources/:id
		// hands the row to the credential decryption in ConnectSource, so a row
		// the scoped read would have withheld is a credential one tenant can
		// decrypt of another's (#393 Phase 2b).
		if h.tenantDualRead {
			h.observeSourceGetScope(c, s)
		}
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
			serverError(c, err, "failed to load source")
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
				serverError(c, dErr, "failed to decrypt source credentials")
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
				serverError(c, err, "failed to encrypt credentials")
				return
			}
			src.EncryptedCredentials = enc
		}

		// SCOPED. Without the organization predicate this UPDATE finds its row by
		// id alone, so a caller holding sources:manage in ANY organization could
		// rewrite another organization's source — including its connector config
		// and stored credentials.
		scope, resolved := tenantscope.FromContext(c)
		if !resolved {
			serverError(c, errNoTenantScope, "the tenant scope was not resolved for this route")
			return
		}
		updated, err := h.repo.UpdateInScope(c.Request.Context(), src, scope)
		if errors.Is(err, repositories.ErrNotInScope) {
			// Rendered as a missing row, not a 403: see tenant_write_scope.go.
			c.JSON(http.StatusNotFound, gin.H{"error": "source not found"})
			return
		}
		if err != nil {
			serverError(c, err, "failed to update source")
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

// TestSourceConfig validates connectivity for an UNSAVED source configuration:
// it builds the connector from the request body and lists its states, without
// persisting anything. This is the main-app counterpart to the setup wizard's
// POST /setup/sources/test, so the add-source dialog can test before saving.
// When source_id is set and credentials are blank, the stored source's
// credentials are reused — the edit-dialog contract (blank = keep existing),
// mirroring UpdateSource's validation path.
// @Summary      Test an unsaved source configuration
// @Description  Builds a connector from the request body and lists its states; nothing is persisted. With source_id and blank credentials, the stored credentials are reused. Requires sources:manage.
// @Tags         Sources
// @Accept       json
// @Produce      json
// @Success      200  {object}  map[string]interface{}
// @Failure      400  {object}  map[string]interface{}
// @Failure      404  {object}  map[string]interface{}
// @Failure      502  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     CookieAuth
// @Router       /sources/test [post]
func (h *SourcesHandlers) TestSourceConfig() gin.HandlerFunc {
	return func(c *gin.Context) {
		var req struct {
			Type        string         `json:"type" binding:"required"`
			Config      map[string]any `json:"config"`
			Credentials map[string]any `json:"credentials"`
			SourceID    string         `json:"source_id"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"status": "failed", "error": "type is required"})
			return
		}

		creds := req.Credentials
		if len(creds) == 0 && req.SourceID != "" {
			existing, err := h.repo.GetByID(c.Request.Context(), req.SourceID)
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"status": "failed", "error": "failed to load source"})
				return
			}
			if existing == nil {
				c.JSON(http.StatusNotFound, gin.H{"status": "failed", "error": "source not found"})
				return
			}
			stored, dErr := decryptCredentials(existing)
			if dErr != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"status": "failed", "error": "failed to decrypt source credentials"})
				return
			}
			creds = stored
		}

		conn, err := statesource.New(req.Type, req.Config, creds)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"status": "failed", "error": err.Error()})
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
		// SCOPED, and this is the destructive one: state_sources cascades to
		// state_backups, state_edits, state_locks, state_analyses,
		// source_sync_status, state_analysis_history, state_module_refs and
		// state_transfers. Deleting by id alone let a caller holding
		// sources:manage in ANY organization destroy another organization's
		// source and everything hanging off it.
		scope, resolved := tenantscope.FromContext(c)
		if !resolved {
			serverError(c, errNoTenantScope, "the tenant scope was not resolved for this route")
			return
		}
		deleted, err := h.repo.DeleteInScope(c.Request.Context(), id, scope)
		if errors.Is(err, repositories.ErrNotInScope) {
			c.JSON(http.StatusNotFound, gin.H{"error": "source not found"})
			return
		}
		if err != nil {
			serverError(c, err, "failed to delete source")
			return
		}
		if !deleted {
			// A 204 here would report success for a delete that removed nothing,
			// which is how a caller learns their id was wrong OR belonged to
			// somebody else. Both are "not found".
			c.JSON(http.StatusNotFound, gin.H{"error": "source not found"})
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
			upstreamError(c, http.StatusBadGateway, err, "failed to list states from the backend")
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
			serverError(c, err, "failed to load analysis history")
			return
		}
		c.JSON(http.StatusOK, gin.H{"key": key, "history": history})
	}
}

// ListStateModules returns the registry modules captured from ingested plans for
// a source, optionally narrowed to one state via ?key=. The list is empty when
// no provenance has been captured (normal — capture only happens when a full
// plan is pushed to /drift/ingest), not an error.
// @Summary      List a source's captured module provenance
// @Tags         Sources
// @Produce      json
// @Param        id   path   string  true   "Source ID"
// @Param        key  query  string  false  "Restrict to a single state key"
// @Success      200  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     CookieAuth
// @Router       /sources/{id}/modules [get]
func (h *SourcesHandlers) ListStateModules() gin.HandlerFunc {
	return func(c *gin.Context) {
		mods, err := h.moduleRefRepo.ListBySource(c.Request.Context(), c.Param("id"), c.Query("key"))
		if err != nil {
			serverError(c, err, "failed to load module provenance")
			return
		}
		c.JSON(http.StatusOK, gin.H{"modules": mods})
	}
}

// Consumers returns the states that consume a given registry module — the
// cross-app read surface a sibling registry server-proxies to power its
// "Consumed by" panel. Both host and module are required; results are matched on
// (registry_host, module_source) so a local module named like a public one never
// produces a false join.
// @Summary      List states consuming a registry module
// @Tags         Sources
// @Produce      json
// @Param        host    query  string  true  "Registry host, e.g. registry.terraform.io"
// @Param        module                 query   string  true  "Module source as namespace/name/system"
// @Param        X-Suite-Service-Token  header  string  true  "Shared suite service token (server-to-server cross-app read)"
// @Success      200  {object}  map[string]interface{}
// @Failure      400  {object}  map[string]interface{}
// @Failure      401  {object}  map[string]interface{}
// @Router       /consumers [get]
func (h *SourcesHandlers) Consumers() gin.HandlerFunc {
	return func(c *gin.Context) {
		module := c.Query("module")
		// A registry may emit several acceptable host identities (its public host,
		// its discovery host, plus operator-configured aliases) as repeated
		// ?host= params. Canonicalize + de-dup them so the exact-match join is
		// symmetric with the form captured from module source addresses and
		// tolerant of vanity-CNAME / port-asymmetry deployments. A single ?host=
		// (the pre-alias contract) still works — QueryArray returns one element.
		seen := map[string]struct{}{}
		hosts := make([]string, 0, len(c.QueryArray("host")))
		for _, raw := range c.QueryArray("host") {
			ch := suite.CanonicalHost(raw)
			if ch == "" {
				continue
			}
			if _, dup := seen[ch]; dup {
				continue
			}
			seen[ch] = struct{}{}
			hosts = append(hosts, ch)
		}
		if len(hosts) == 0 || module == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "host and module query parameters are required"})
			return
		}
		consumers, err := h.moduleRefRepo.FindConsumers(c.Request.Context(), hosts, module)
		if err != nil {
			serverError(c, err, "failed to load consumers")
			return
		}
		c.JSON(http.StatusOK, gin.H{"consumers": consumers, "total": len(consumers)})
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
		serverError(c, err, "failed to load source")
		return nil, false
	}
	if s == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "source not found"})
		return nil, false
	}
	creds, err := decryptCredentials(s)
	if err != nil {
		serverError(c, err, "failed to decrypt source credentials")
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
		upstreamError(c, http.StatusBadGateway, err, "failed to read state from the backend")
		return nil, false
	}
	return rs, true
}
