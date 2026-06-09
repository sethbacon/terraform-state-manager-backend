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
	"github.com/terraform-state-manager/terraform-state-manager/internal/api/scim"
	"github.com/terraform-state-manager/terraform-state-manager/internal/auth"
	"github.com/terraform-state-manager/terraform-state-manager/internal/auth/mtls"
	"github.com/terraform-state-manager/terraform-state-manager/internal/config"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
	"github.com/terraform-state-manager/terraform-state-manager/internal/middleware"
	"github.com/terraform-state-manager/terraform-state-manager/internal/services/scheduler"
)

// NewRouter builds the application's HTTP handler. database is the app/public
// connection; identityDB resolves to the identity schema (search_path) for the
// shared identity repositories used by auth. It also starts the background
// schedule runner (when database is non-nil) and returns a stop func the caller
// must invoke on shutdown to halt it.
func NewRouter(cfg *config.Config, database *sql.DB, identityDB *sql.DB) (*gin.Engine, func(), error) {
	stop := func() {} // halts background workers; replaced when the scheduler starts
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
			return nil, stop, fmt.Errorf("failed to initialise mTLS provider: %w", err)
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
		return nil, stop, err
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
			// Read-only view of configured SSO/identity providers + mappings.
			ag.GET("/sso", authHandlers.SSOConfigHandler())
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

		// Scheduler: cron-driven schedules that dispatch drift runs. The same drift
		// dispatcher backs the HTTP "run now" endpoint and the background runner.
		driftDisp := driftDispatcher{drift: drift}
		scheduleHandlers := NewScheduleHandlers(database, driftDisp)
		sg := v1.Group("/schedules", requireAuth)
		{
			sg.GET("", middleware.RequireScope(auth.ScopeStateRead), scheduleHandlers.ListSchedules())
			sg.POST("", middleware.RequireScope(auth.ScopeSourcesManage), scheduleHandlers.CreateSchedule())
			sg.GET("/:id", middleware.RequireScope(auth.ScopeStateRead), scheduleHandlers.GetSchedule())
			sg.PUT("/:id", middleware.RequireScope(auth.ScopeSourcesManage), scheduleHandlers.UpdateSchedule())
			sg.DELETE("/:id", middleware.RequireScope(auth.ScopeSourcesManage), scheduleHandlers.DeleteSchedule())
			sg.POST("/:id/run", middleware.RequireScope(auth.ScopeSourcesManage), scheduleHandlers.RunSchedule())
		}

		// Start the background runner. Guarded on database so unit tests that build
		// the router with a nil DB don't spin up a goroutine that would hit it.
		if database != nil {
			runner := scheduler.New(repositories.NewScheduleRepository(database), driftDisp)
			runner.Start()
			stop = runner.Stop
		}
	}

	// SCIM 2.0 provisioning (RFC 7644), mounted at the conventional top-level
	// /scim/v2 and only when enabled. Bearer-token auth + scim:provision scope
	// (admin satisfies it); no cookie auth, so it is outside the CSRF-protected
	// /api/v1 group. IdP clients present Authorization: Bearer <token>.
	if cfg.Auth.SCIM.Enabled {
		scimHandlers := scim.NewHandlers(cfg, identityDB)
		sc := r.Group("/scim/v2", requireAuth, middleware.RequireScope(auth.ScopeSCIMProvision))
		{
			sc.GET("/Users", scimHandlers.ListUsers())
			sc.GET("/Users/:id", scimHandlers.GetUser())
			sc.POST("/Users", scimHandlers.CreateUser())
			sc.PUT("/Users/:id", scimHandlers.PutUser())
			sc.PATCH("/Users/:id", scimHandlers.PatchUser())
			sc.DELETE("/Users/:id", scimHandlers.DeleteUser())
			sc.GET("/Groups", scimHandlers.ListGroups())
			sc.GET("/Groups/:id", scimHandlers.GetGroup())
		}
	}

	return r, stop, nil
}
