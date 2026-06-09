// Package api wires the Gin router: middleware chain plus the HTTP handlers.
// Phase 0 exposes system endpoints (health, readiness, version); authentication
// (OIDC + dev login) is layered on here. Domain route groups (sources, states,
// drift, health runs, transfers) are registered as those features land.
package api

import (
	"database/sql"
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"

	"github.com/terraform-state-manager/terraform-state-manager/docs"
	"github.com/terraform-state-manager/terraform-state-manager/internal/auth"
	"github.com/terraform-state-manager/terraform-state-manager/internal/auth/mtls"
	"github.com/terraform-state-manager/terraform-state-manager/internal/config"
	"github.com/terraform-state-manager/terraform-state-manager/internal/middleware"
)

// NewRouter builds the application's HTTP handler. database is the app/public
// connection; identityDB resolves to the identity schema (search_path) for the
// shared identity repositories used by auth.
func NewRouter(cfg *config.Config, database *sql.DB, identityDB *sql.DB) (*gin.Engine, error) {
	r := gin.New()
	r.Use(gin.Recovery())
	r.Use(middleware.RequestID())
	r.Use(middleware.SecurityHeaders())
	r.Use(middleware.Metrics())

	// mTLS: when enabled, a verified client cert (against the configured client CA)
	// authenticates the request additively, before JWT auth. No-op if not enabled
	// or if the TLS layer didn't verify a client cert.
	if cfg.Auth.MTLS.Enabled {
		mtlsProvider, err := mtls.NewProvider(cfg.Auth.MTLS)
		if err != nil {
			return nil, fmt.Errorf("failed to initialise mTLS provider: %w", err)
		}
		r.Use(mtls.AuthMiddleware(mtlsProvider))
	}

	// System endpoints (unversioned; used by orchestrators and probes).
	r.GET("/health", health)
	r.GET("/ready", ready(database))

	// OpenAPI spec (unauthenticated, read-only) — generated from handler swag
	// annotations (see docs package). Rendered by the frontend's API docs page.
	r.GET("/swagger.json", func(c *gin.Context) {
		c.Data(http.StatusOK, "application/json; charset=utf-8", docs.SwaggerJSON)
	})
	r.GET("/swagger.yaml", func(c *gin.Context) {
		c.Data(http.StatusOK, "application/yaml; charset=utf-8", docs.SwaggerYAML)
	})

	authHandlers, err := NewAuthHandlers(cfg, identityDB)
	if err != nil {
		return nil, err
	}
	requireAuth := middleware.AuthMiddleware(authHandlers.UserRepo(), authHandlers.TokenRepo())
	optionalAuth := middleware.OptionalAuthMiddleware(authHandlers.UserRepo(), authHandlers.TokenRepo())

	v1 := r.Group("/api/v1")
	// Enforce double-submit CSRF on cookie-authenticated, state-changing requests.
	v1.Use(middleware.CSRFProtect())
	{
		v1.GET("/version", version)

		a := v1.Group("/auth")
		{
			a.GET("/providers", authHandlers.ProvidersHandler())
			a.GET("/login", authHandlers.LoginHandler())
			a.GET("/callback", authHandlers.CallbackHandler())
			a.POST("/ldap/login", authHandlers.LDAPLoginHandler())
			// SAML 2.0 SP endpoints: SP metadata and the Assertion Consumer Service.
			a.GET("/saml/metadata", authHandlers.SAMLMetadataHandler())
			a.POST("/saml/acs", authHandlers.SAMLACSHandler())
			// Authenticated session endpoints.
			a.GET("/me", requireAuth, authHandlers.MeHandler())
			a.POST("/refresh", requireAuth, authHandlers.RefreshHandler())
			a.GET("/logout", optionalAuth, authHandlers.LogoutHandler())
		}

		// Development-only login (guarded again inside the handler).
		if auth.IsDevMode() {
			v1.POST("/dev/login", authHandlers.DevLoginHandler())
		}

		// Ad-hoc analysis of an uploaded state file (no stored source).
		v1.POST("/analyze", requireAuth, middleware.RequireScope(auth.ScopeStateRead), AnalyzeUploadHandler())

		// Phase 1 read plane: state sources + analysis.
		sources := NewSourcesHandlers(database)
		s := v1.Group("/sources", requireAuth)
		{
			s.GET("", middleware.RequireScope(auth.ScopeStateRead), sources.ListSources())
			s.POST("", middleware.RequireScope(auth.ScopeSourcesManage), sources.CreateSource())
			s.GET("/:id", middleware.RequireScope(auth.ScopeStateRead), sources.GetSource())
			s.DELETE("/:id", middleware.RequireScope(auth.ScopeSourcesManage), sources.DeleteSource())
			s.GET("/:id/states", middleware.RequireScope(auth.ScopeStateRead), sources.ListStates())
			s.GET("/:id/state/analysis", middleware.RequireScope(auth.ScopeStateRead), sources.AnalyzeState())
			s.GET("/:id/state/raw", middleware.RequireScope(auth.ScopeStateRead), sources.RawState())
			s.GET("/:id/state/resources", middleware.RequireScope(auth.ScopeStateRead), sources.ListStateResources())
			s.GET("/:id/state/report", middleware.RequireScope(auth.ScopeStateRead), sources.StateReport())

			// Phase 2 edit plane (validate → backup → write → audit; restore).
			s.PUT("/:id/state/raw", middleware.RequireScope(auth.ScopeStateWrite), sources.EditState())
			s.POST("/:id/state/operations", middleware.RequireScope(auth.ScopeStateWrite), sources.StateOperation())
			s.GET("/:id/state/backups", middleware.RequireScope(auth.ScopeStateRead), sources.ListBackups())
			s.POST("/:id/state/backups/:backupId/restore", middleware.RequireScope(auth.ScopeStateWrite), sources.RestoreBackup())

			// Phase 2 transfer plane: cross-source backup (copy) and migrate (move).
			s.POST("/:id/state/backup", middleware.RequireScope(auth.ScopeStateTransfer), sources.BackupToSource())
			s.POST("/:id/state/migrate", middleware.RequireScope(auth.ScopeStateTransfer), sources.MigrateToSource())
		}

		v1.GET("/transfers/:id", requireAuth, middleware.RequireScope(auth.ScopeStateRead), sources.GetTransfer())

		// Home dashboard: cross-source aggregated overview.
		v1.GET("/dashboard/overview", requireAuth, middleware.RequireScope(auth.ScopeStateRead), sources.DashboardOverview())

		// Identity management (admin scope): users, organizations, roles, audit log.
		admin := NewAdminHandlers(sqlx.NewDb(identityDB, "postgres"))
		ag := v1.Group("/admin", requireAuth, middleware.RequireScope(auth.ScopeAdmin))
		{
			ag.GET("/stats", admin.Stats())
			ag.GET("/users", admin.ListUsers())
			ag.GET("/organizations", admin.ListOrganizations())
			ag.GET("/roles", admin.ListRoles())
			ag.GET("/audit-logs", admin.ListAuditLogs())
		}

		// Phase 3 drift: CI pipeline connections + drift runs.
		drift := NewDriftHandlers(cfg, database)
		p := v1.Group("/pipelines", requireAuth)
		{
			p.GET("", middleware.RequireScope(auth.ScopeSourcesManage), drift.ListPipelines())
			p.POST("", middleware.RequireScope(auth.ScopeSourcesManage), drift.CreatePipeline())
			p.DELETE("/:id", middleware.RequireScope(auth.ScopeSourcesManage), drift.DeletePipeline())
		}
		d := v1.Group("/drift", requireAuth)
		{
			d.GET("/workflow", middleware.RequireScope(auth.ScopeStateRead), drift.WorkflowTemplate())
			d.POST("/runs", middleware.RequireScope(auth.ScopeStateDrift), drift.CreateRun())
			d.GET("/runs", middleware.RequireScope(auth.ScopeStateRead), drift.ListRuns())
			d.GET("/runs/:id", middleware.RequireScope(auth.ScopeStateRead), drift.GetRun())
		}
		// Machine callback (authenticated by the per-run token, not a user session).
		v1.POST("/drift/runs/:id/results", drift.RunResults())

		// Phase 4 version lab: dispatch plan against pinned versions + health.
		health := NewHealthHandlers(cfg, database)
		hg := v1.Group("/health-lab", requireAuth)
		{
			hg.GET("/workflow", middleware.RequireScope(auth.ScopeStateRead), health.WorkflowTemplate())
			hg.POST("/runs", middleware.RequireScope(auth.ScopeStateExecute), health.CreateRun())
			hg.GET("/runs", middleware.RequireScope(auth.ScopeStateRead), health.ListRuns())
			hg.GET("/runs/:id", middleware.RequireScope(auth.ScopeStateRead), health.GetRun())
		}
		v1.POST("/health-lab/runs/:id/results", health.RunResults())
	}

	return r, nil
}
