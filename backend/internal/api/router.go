// Package api wires the Gin router: middleware chain plus the HTTP handlers.
// Phase 0 exposes system endpoints (health, readiness, version); authentication
// (OIDC + dev login) is layered on here. Domain route groups (sources, states,
// drift, health runs, transfers) are registered as those features land.
package api

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"
	"github.com/sethbacon/terraform-suite-identity/identity/suite"

	"github.com/terraform-state-manager/terraform-state-manager/docs"
	"github.com/terraform-state-manager/terraform-state-manager/internal/api/scim"
	"github.com/terraform-state-manager/terraform-state-manager/internal/api/setup"
	"github.com/terraform-state-manager/terraform-state-manager/internal/auth"
	"github.com/terraform-state-manager/terraform-state-manager/internal/auth/mtls"
	"github.com/terraform-state-manager/terraform-state-manager/internal/config"
	"github.com/terraform-state-manager/terraform-state-manager/internal/crypto"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
	"github.com/terraform-state-manager/terraform-state-manager/internal/middleware"
	"github.com/terraform-state-manager/terraform-state-manager/internal/services/notify"
	"github.com/terraform-state-manager/terraform-state-manager/internal/services/scheduler"
	"github.com/terraform-state-manager/terraform-state-manager/internal/services/statesync"
)

// NewRouter builds the application's HTTP handler. database is the app/public
// connection; identityDB resolves to the identity schema (search_path) for the
// shared identity repositories used by auth. It also starts the background
// schedule runner (when database is non-nil) and returns a stop func the caller
// must invoke on shutdown to halt it.
func NewRouter(cfg *config.Config, database *sql.DB, identityDB *sql.DB) (*gin.Engine, func(), error) {
	stop := func() {} // halts background workers; replaced when the scheduler starts
	r := gin.New()
	if err := r.SetTrustedProxies(cfg.Server.TrustedProxies); err != nil {
		return nil, stop, fmt.Errorf("invalid trusted_proxies: %w", err)
	}
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

	// Load any DB-stored OIDC config (written by the setup wizard) into the live
	// auth handler, so OIDC configured via the wizard survives a restart without
	// needing config-file settings. Takes precedence over an empty config.
	oidcConfigRepo := repositories.NewOIDCConfigRepository(database)
	if database != nil {
		if active, oerr := oidcConfigRepo.GetActiveOIDCConfig(context.Background()); oerr == nil && active != nil {
			if secret, derr := crypto.Decrypt(active.ClientSecretEncrypted); derr != nil {
				slog.Error("failed to decrypt stored OIDC client secret", "error", derr)
			} else if provider, perr := auth.NewOIDCProvider(&config.OIDCConfig{
				Enabled:      true,
				IssuerURL:    active.IssuerURL,
				ClientID:     active.ClientID,
				ClientSecret: string(secret),
				RedirectURL:  active.RedirectURL,
				Scopes:       active.Scopes,
			}); perr != nil {
				slog.Error("failed to build OIDC provider from stored config", "error", perr)
			} else {
				authHandlers.SetOIDCProvider(provider)
				slog.Info("OIDC provider loaded from database configuration")
			}
		}
	}
	requireAuth := middleware.AuthMiddleware(authHandlers.UserRepo(), authHandlers.TokenRepo(), authHandlers.APIKeyRepo())
	optionalAuth := middleware.OptionalAuthMiddleware(authHandlers.UserRepo(), authHandlers.TokenRepo())

	var suiteClient *suite.DiscoveryClient

	v1 := r.Group("/api/v1")
	// Enforce double-submit CSRF on cookie-authenticated, state-changing requests.
	v1.Use(middleware.CSRFProtect())
	{
		v1.GET("/version", version)
		v1.GET("/suite/manifest", suiteManifestHandler(cfg))
		v1.GET("/ui/config", uiConfigHandler(cfg, func() *suite.DiscoveryClient { return suiteClient }))

		// First-run setup wizard. Status is public so the SPA can decide whether
		// to show the wizard; mutating endpoints sit behind the setup-token
		// middleware and are permanently disabled once setup completes. CSRF is a
		// no-op here (no session cookie exists during first-run setup).
		settingsRepo := repositories.NewSystemSettingsRepository(database)
		setupHandlers := setup.NewHandlers(settingsRepo, oidcConfigRepo, repositories.NewSourceRepository(database), identityDB, cfg, authHandlers.SetOIDCProvider)
		v1.GET("/setup/status", setupHandlers.GetSetupStatus)
		setupGroup := v1.Group("/setup")
		setupGroup.Use(middleware.SetupTokenMiddleware(settingsRepo))
		{
			setupGroup.POST("/validate-token", setupHandlers.ValidateToken)
			setupGroup.POST("/admin", setupHandlers.ConfigureAdmin)
			setupGroup.POST("/oidc/test", setupHandlers.TestOIDCConfig)
			setupGroup.POST("/oidc", setupHandlers.SaveOIDCConfig)
			setupGroup.POST("/sources/test", setupHandlers.TestSource)
			setupGroup.POST("/sources", setupHandlers.SaveSource)
			setupGroup.POST("/complete", setupHandlers.CompleteSetup)
		}

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

		// API keys (registry-modeled self-service): any authenticated user
		// manages their own keys; admin sees all. No extra scope gate — the
		// handlers enforce ownership and scope-grant limits themselves.
		apiKeys := NewAPIKeysHandlers(identityDB)
		ak := v1.Group("/apikeys", requireAuth)
		{
			ak.GET("", apiKeys.ListAPIKeys())
			ak.POST("", apiKeys.CreateAPIKey())
			ak.GET("/:id", apiKeys.GetAPIKey())
			ak.PUT("/:id", apiKeys.UpdateAPIKey())
			ak.DELETE("/:id", apiKeys.DeleteAPIKey())
			ak.POST("/:id/rotate", apiKeys.RotateAPIKey())
		}

		// Phase 1 read plane: state sources + analysis.
		sources := NewSourcesHandlers(database, identityDB)
		s := v1.Group("/sources", requireAuth)
		{
			s.GET("", middleware.RequireScope(auth.ScopeStateRead), sources.ListSources())
			s.POST("", middleware.RequireScope(auth.ScopeSourcesManage), sources.CreateSource())
			s.GET("/:id", middleware.RequireScope(auth.ScopeStateRead), sources.GetSource())
			s.PUT("/:id", middleware.RequireScope(auth.ScopeSourcesManage), sources.UpdateSource())
			s.DELETE("/:id", middleware.RequireScope(auth.ScopeSourcesManage), sources.DeleteSource())
			s.POST("/:id/test", middleware.RequireScope(auth.ScopeStateRead), sources.TestSource())
			s.GET("/:id/states", middleware.RequireScope(auth.ScopeStateRead), sources.ListStates())
			s.GET("/:id/state/analysis", middleware.RequireScope(auth.ScopeStateRead), sources.AnalyzeState())
			s.GET("/:id/state/raw", middleware.RequireScope(auth.ScopeStateRead), sources.RawState())
			s.GET("/:id/state/resources", middleware.RequireScope(auth.ScopeStateRead), sources.ListStateResources())
			s.GET("/:id/state/outputs", middleware.RequireScope(auth.ScopeStateRead), sources.StateOutputs())
			s.GET("/:id/state/history", middleware.RequireScope(auth.ScopeStateRead), sources.StateHistory())
			s.GET("/:id/state/report", middleware.RequireScope(auth.ScopeStateRead), sources.StateReport())
			s.GET("/:id/modules", middleware.RequireScope(auth.ScopeStateRead), sources.ListStateModules())

			// Phase 2 edit plane (validate → backup → write → audit; restore).
			s.PUT("/:id/state/raw", middleware.RequireScope(auth.ScopeStateWrite), sources.EditState())
			s.POST("/:id/state/operations", middleware.RequireScope(auth.ScopeStateWrite), sources.StateOperation())
			s.GET("/:id/state/backups", middleware.RequireScope(auth.ScopeStateRead), sources.ListBackups())
			s.POST("/:id/state/backups/:backupId/restore", middleware.RequireScope(auth.ScopeStateWrite), sources.RestoreBackup())
			s.DELETE("/:id/state/lock", middleware.RequireScope(auth.ScopeAdmin), sources.ForceUnlock())

			// Phase 2 transfer plane: cross-source backup (copy) and migrate (move).
			s.POST("/:id/state/backup", middleware.RequireScope(auth.ScopeStateTransfer), sources.BackupToSource())
			s.POST("/:id/state/migrate", middleware.RequireScope(auth.ScopeStateTransfer), sources.MigrateToSource())
		}

		v1.GET("/transfers/:id", requireAuth, middleware.RequireScope(auth.ScopeStateRead), sources.GetTransfer())

		// Cross-app: states consuming a given registry module (a sibling registry
		// server-proxies to this to power its "Consumed by" panel).
		v1.GET("/consumers", middleware.RequireSuiteServiceToken(cfg.Suite.ServiceToken), sources.Consumers())

		// Cross-app: a sibling app federates its audit entries here (shared-store
		// only — enforced in the handler, and advertised via audit.ingest.v1).
		auditIngest := NewAuditIngestHandlers(identityDB, cfg)
		v1.POST("/audit/ingest", middleware.RequireSuiteServiceToken(cfg.Suite.ServiceToken), auditIngest.Ingest())

		// Home dashboard: cross-source aggregated overview.
		v1.GET("/dashboard/overview", requireAuth, middleware.RequireScope(auth.ScopeStateRead), sources.DashboardOverview())

		// Identity management (admin scope): users, organizations, roles, audit log.
		admin := NewAdminHandlers(sqlx.NewDb(identityDB, "postgres"))
		ag := v1.Group("/admin", requireAuth, middleware.RequireScope(auth.ScopeAdmin))
		{
			ag.GET("/stats", admin.Stats())

			// Users: list/search + CRUD + memberships + GDPR data-subject actions.
			ag.GET("/users", admin.ListUsers())
			ag.POST("/users", admin.CreateUser())
			ag.PUT("/users/:id", admin.UpdateUser())
			ag.DELETE("/users/:id", admin.DeleteUser())
			ag.GET("/users/:id/memberships", admin.GetUserMemberships())
			ag.GET("/users/:id/export", admin.ExportUserData())
			ag.POST("/users/:id/erase", admin.EraseUser())

			// Organizations: CRUD + member management.
			ag.GET("/organizations", admin.ListOrganizations())
			ag.POST("/organizations", admin.CreateOrganization())
			ag.PUT("/organizations/:id", admin.UpdateOrganization())
			ag.DELETE("/organizations/:id", admin.DeleteOrganization())
			ag.GET("/organizations/:id/members", admin.ListOrganizationMembers())
			ag.POST("/organizations/:id/members", admin.AddOrganizationMember())
			ag.PUT("/organizations/:id/members/:user_id", admin.UpdateOrganizationMember())
			ag.DELETE("/organizations/:id/members/:user_id", admin.RemoveOrganizationMember())

			ag.GET("/roles", admin.ListRoles())
			ag.GET("/audit-logs", admin.ListAuditLogs())

			// Read-only view of configured SSO/identity providers + mappings.
			ag.GET("/sso", authHandlers.SSOConfigHandler())
			// OIDC group-mapping settings (runtime-editable overlay) + read-only
			// SAML/LDAP mappings and mTLS configuration.
			ag.GET("/oidc/config", authHandlers.OIDCConfigHandler())
			ag.PUT("/oidc/group-mapping", authHandlers.UpdateOIDCGroupMapping())
			ag.GET("/identity-group-mappings", authHandlers.IdentityGroupMappingsHandler())
			ag.GET("/mtls", authHandlers.MTLSConfigHandler())
		}

		// Notifier fans drift/failure alerts out to configured channels. Nil with a
		// nil DB (unit tests) — the drift handler treats a nil notifier as a no-op.
		var notifier *notify.Notifier
		if database != nil {
			notifier = notify.New(repositories.NewNotificationChannelRepository(database))
		}

		// Phase 3 drift: CI pipeline connections + drift runs.
		drift := NewDriftHandlers(cfg, database, identityDB, notifier)
		p := v1.Group("/pipelines", requireAuth)
		{
			p.GET("", middleware.RequireScope(auth.ScopeSourcesManage), drift.ListPipelines())
			p.POST("", middleware.RequireScope(auth.ScopeSourcesManage), drift.CreatePipeline())
			p.DELETE("/:id", middleware.RequireScope(auth.ScopeSourcesManage), drift.DeletePipeline())
			// Repo-setup wizard preflight: is the callback URL reachable from CI?
			p.GET("/callback-preflight", middleware.RequireScope(auth.ScopeSourcesManage), drift.CallbackPreflight())
		}

		// CI sources: org-level CI credentials + pipeline/repo/workflow discovery,
		// so pipeline connections can be created by selection.
		ciSources := NewCISourceHandlers(database, identityDB)
		cs := v1.Group("/ci-sources", requireAuth, middleware.RequireScope(auth.ScopeSourcesManage))
		{
			cs.GET("", ciSources.ListCISources())
			cs.POST("", ciSources.CreateCISource())
			cs.DELETE("/:id", ciSources.DeleteCISource())
			cs.GET("/:id/pipelines", ciSources.ListSourcePipelines())
			cs.GET("/:id/repos", ciSources.ListSourceRepos())
			cs.GET("/:id/repos/:repo/workflows", ciSources.ListSourceWorkflows())
			// Repo-setup wizard: ADO service connections + pipeline creation.
			cs.GET("/:id/service-connections", ciSources.ListSourceServiceConnections())
			cs.POST("/:id/repos/:repo/pipelines", ciSources.CreateSourcePipeline())
			// Phase 2: commit the workflow via branch + PR, and poll the PR state.
			cs.POST("/:id/repos/:repo/workflow-setup", ciSources.SetupSourceWorkflow())
			cs.GET("/:id/repos/:repo/prs/:pr", ciSources.GetSourcePRState())
		}
		d := v1.Group("/drift", requireAuth)
		{
			d.GET("/workflow", middleware.RequireScope(auth.ScopeStateRead), drift.WorkflowTemplate())
			d.POST("/runs", middleware.RequireScope(auth.ScopeStateDrift), drift.CreateRun())
			d.GET("/runs", middleware.RequireScope(auth.ScopeStateRead), drift.ListRuns())
			d.GET("/runs/:id", middleware.RequireScope(auth.ScopeStateRead), drift.GetRun())

			// Drift records: the durable, acknowledgeable layer over runs, plus
			// push-style ingest for pipelines TSM did not dispatch.
			d.POST("/ingest", middleware.RequireScope(auth.ScopeStateDrift), drift.IngestDrift())
			d.GET("/records", middleware.RequireScope(auth.ScopeStateRead), drift.ListDriftRecords())
			d.GET("/records/:id", middleware.RequireScope(auth.ScopeStateRead), drift.GetDriftRecord())
			d.POST("/records/:id/acknowledge", middleware.RequireScope(auth.ScopeStateDrift), drift.AcknowledgeDriftRecord())
			d.POST("/records/:id/resolve", middleware.RequireScope(auth.ScopeStateDrift), drift.ResolveDriftRecord())
		}
		// Machine callback (authenticated by the per-run token, not a user session).
		v1.POST("/drift/runs/:id/results", drift.RunResults())

		// Phase 4 version lab: dispatch plan against pinned versions + health.
		health := NewHealthHandlers(cfg, database, identityDB)
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
		scheduleHandlers := NewScheduleHandlers(database, identityDB, driftDisp)
		sg := v1.Group("/schedules", requireAuth)
		{
			sg.GET("", middleware.RequireScope(auth.ScopeStateRead), scheduleHandlers.ListSchedules())
			sg.POST("", middleware.RequireScope(auth.ScopeSourcesManage), scheduleHandlers.CreateSchedule())
			sg.GET("/:id", middleware.RequireScope(auth.ScopeStateRead), scheduleHandlers.GetSchedule())
			sg.PUT("/:id", middleware.RequireScope(auth.ScopeSourcesManage), scheduleHandlers.UpdateSchedule())
			sg.DELETE("/:id", middleware.RequireScope(auth.ScopeSourcesManage), scheduleHandlers.DeleteSchedule())
			sg.POST("/:id/run", middleware.RequireScope(auth.ScopeSourcesManage), scheduleHandlers.RunSchedule())
		}

		// Notification channels (admin): alert destinations + the drift-event hook.
		// Target URLs are secrets, so the whole group is admin-scoped.
		notif := NewNotificationHandlers(database, identityDB, notifier)
		ng := v1.Group("/notifications", requireAuth, middleware.RequireScope(auth.ScopeAdmin))
		{
			ng.GET("/channels", notif.ListChannels())
			ng.POST("/channels", notif.CreateChannel())
			ng.PUT("/channels/:id", notif.UpdateChannel())
			ng.DELETE("/channels/:id", notif.DeleteChannel())
			ng.POST("/channels/:id/test", notif.TestChannel())
		}

		// Background workers. Guarded on database so unit tests that build the
		// router with a nil DB don't spin up goroutines that would hit it. The
		// syncer OBJECT is always attached (post-write refreshes and source-create
		// backfills must work on every replica); the PERIODIC loops — schedule
		// runner + statesync reconcile — start only when workers are enabled, so
		// multi-replica deployments can scale API pods while exactly one dedicated
		// worker replica fires schedules (GetDue has no cross-replica claim).
		if database != nil {
			syncer := statesync.New(
				repositories.NewSourceRepository(database),
				repositories.NewStateAnalysisRepository(database),
				ConnectSource,
			)
			sources.AttachSyncer(syncer)
			if cfg.Workers.Enabled {
				runner := scheduler.New(repositories.NewScheduleRepository(database), driftDisp)
				runner.Start()
				syncer.Start()
				runnerStop := runner.Stop
				stop = func() {
					runnerStop()
					syncer.Stop()
				}
			} else {
				slog.Info("background workers disabled on this replica (workers.enabled=false); " +
					"schedule firing and periodic state sync run on the dedicated worker")
			}
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

	if cfg.Suite.SiblingURL != "" {
		dc := suite.NewDiscoveryClient(cfg.Suite.SiblingURL, buildSuiteManifest(cfg), cfg.Suite.PollInterval)
		ctx, cancel := context.WithCancel(context.Background())
		go dc.Start(ctx)
		suiteClient = dc
		prevStop := stop
		stop = func() { cancel(); prevStop() }
	}

	return r, stop, nil
}
