// Package api wires the Gin router: middleware chain plus the HTTP handlers.
// Phase 0 exposes system endpoints (health, readiness, version); authentication
// (OIDC + dev login) is layered on here. Domain route groups (sources, states,
// drift, health runs, transfers) are registered as those features land.
package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"

	identitycrypto "github.com/sethbacon/terraform-suite-identity/identity/crypto"
	identitymailer "github.com/sethbacon/terraform-suite-identity/identity/mailer"
	identitynotify "github.com/sethbacon/terraform-suite-identity/identity/notify"
	idstore "github.com/sethbacon/terraform-suite-identity/identity/store"
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
	"github.com/terraform-state-manager/terraform-state-manager/internal/services/driftreconcile"
	"github.com/terraform-state-manager/terraform-state-manager/internal/services/healthreconcile"
	"github.com/terraform-state-manager/terraform-state-manager/internal/services/leaderelect"
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
	r.Use(middleware.AccessLog())

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
	r.GET("/ready", ready(database, identityDB))

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
	requireAuth := middleware.AuthMiddleware(authHandlers.UserRepo(), authHandlers.TokenRepo(), authHandlers.APIKeyRepo(), authHandlers.OrgRepo())
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
		// Whitelabel branding: public read (the login page renders it before any
		// session exists), admin-scoped write. Nil settings on the nil-DB rig.
		themeSettings := settingsRepo
		if database == nil {
			themeSettings = nil
		}
		v1.GET("/ui/theme", GetUITheme(themeSettings))
		if database != nil {
			v1.PUT("/admin/ui/theme", requireAuth, middleware.RequireScope(auth.ScopeAdmin),
				UpdateUITheme(settingsRepo, newAuditor(identityDB)))
		}
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
			// Static /test must not collide with /:id below: gin resolves static
			// segments before params, so POST /sources/test is unambiguous.
			s.POST("/test", middleware.RequireScope(auth.ScopeSourcesManage), sources.TestSourceConfig())
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
			// Freshness: locked module versions vs the sibling registry's latest.
			// Inert when standalone (no active sibling -> every module "no_registry").
			s.GET("/:id/modules/freshness", middleware.RequireScope(auth.ScopeStateRead), sources.ListStateModuleFreshness(func() *suite.DiscoveryClient { return suiteClient }))

			// Phase 2 edit plane (validate → backup → write → audit; restore).
			s.PUT("/:id/state/raw", middleware.RequireScope(auth.ScopeStateWrite), sources.EditState())
			s.POST("/:id/state/operations", middleware.RequireScope(auth.ScopeStateWrite), sources.StateOperation())
			s.GET("/:id/state/backups", middleware.RequireScope(auth.ScopeStateRead), sources.ListBackups())
			s.GET("/:id/state/backups/:backupId", middleware.RequireScope(auth.ScopeStateRead), sources.GetBackupContent())
			s.GET("/:id/state/backups/:backupId/diff", middleware.RequireScope(auth.ScopeStateRead), sources.DiffBackup())
			s.POST("/:id/state/backups/:backupId/restore", middleware.RequireScope(auth.ScopeStateWrite), sources.RestoreBackup())
			s.GET("/:id/state/locks", middleware.RequireScope(auth.ScopeStateRead), sources.ListLocks())
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
		v1.GET("/dashboard/states-by-version", requireAuth, middleware.RequireScope(auth.ScopeStateRead), sources.StatesByVersion())

		// Reports: cross-fleet state-file query, preview, and multi-format export.
		v1.GET("/reports/states", requireAuth, middleware.RequireScope(auth.ScopeStateRead), sources.ReportStates())
		v1.GET("/reports/states/export", requireAuth, middleware.RequireScope(auth.ScopeStateRead), sources.ReportStatesExport())

		// Identity management (admin scope): users, organizations, roles, audit log.
		admin := NewAdminHandlers(sqlx.NewDb(identityDB, "postgres"))
		ag := v1.Group("/admin", requireAuth, middleware.RequireScope(auth.ScopeAdmin))
		{
			ag.GET("/stats", admin.Stats())

			// Users: list/search + CRUD + memberships + GDPR data-subject actions.
			// Every route naming a specific target user (:id), other than list and
			// create, additionally requires the caller to hold admin within at
			// least one organization THAT USER also belongs to — not just the
			// flat/global admin scope the outer /admin group already checked —
			// closing the cross-org privilege escalation the flat scope alone
			// would otherwise allow (mirroring the /organizations/:id gating below).
			ag.GET("/users", admin.ListUsers())
			ag.POST("/users", admin.CreateUser())
			userScoped := ag.Group("/users/:id", admin.requireSharedOrgAdminWithTargetUser())
			{
				userScoped.PUT("", admin.UpdateUser())
				userScoped.DELETE("", admin.DeleteUser())
				userScoped.GET("/memberships", admin.GetUserMemberships())
				userScoped.GET("/export", admin.ExportUserData())
				userScoped.POST("/erase", admin.EraseUser())
			}

			ag.GET("/roles", admin.ListRoles())
			ag.GET("/audit-logs", admin.ListAuditLogs())
			ag.GET("/audit-logs/export", admin.ExportAuditLogs())

			// CI workflow templates: operator edit/add/replace of the drift /
			// version-lab YAML per (provider, kind, profile).
			ciTemplates := NewCITemplateHandlers(database, identityDB)
			ag.GET("/ci/templates", ciTemplates.ListCITemplates())
			ag.GET("/ci/templates/:id", ciTemplates.GetCITemplate())
			ag.POST("/ci/templates", ciTemplates.CreateCITemplate())
			ag.PUT("/ci/templates/:id", ciTemplates.UpdateCITemplate())
			ag.DELETE("/ci/templates/:id", ciTemplates.DeleteCITemplate())

			// Read-only view of configured SSO/identity providers + mappings.
			ag.GET("/sso", authHandlers.SSOConfigHandler())
			// OIDC group-mapping settings (runtime-editable overlay) + read-only
			// SAML/LDAP mappings and mTLS configuration.
			ag.GET("/oidc/config", authHandlers.OIDCConfigHandler())
			ag.PUT("/oidc/group-mapping", authHandlers.UpdateOIDCGroupMapping())
			ag.GET("/identity-group-mappings", authHandlers.IdentityGroupMappingsHandler())
			ag.GET("/mtls", authHandlers.MTLSConfigHandler())
		}

		// Organizations: deliberately NOT nested under the ScopeAdmin-gated ag
		// group above (only requireAuth at the group level) — org management is
		// delegated to org-tier scopes instead of the flat/global admin wildcard,
		// so an org_owner/org_provisioner never needs platform-admin rights just
		// to run their own organization. List/create name no specific target
		// organization, so they're gated by organizations:read/:create directly;
		// every route naming a specific :id, though, still requires the caller to
		// hold organizations:write (or admin) within THAT organization —
		// re-derived per-request via GetUserScopesForOrg (requireOrgScope, see
		// admin_org_scope.go) rather than trusted from the flat scope set — so an
		// org_owner in org A cannot act on org B.
		orgAdmin := v1.Group("/admin/organizations", requireAuth)
		{
			orgAdmin.GET("", middleware.RequireScope(auth.ScopeOrganizationsRead), admin.ListOrganizations())
			orgAdmin.POST("", middleware.RequireScope(auth.ScopeOrganizationsCreate), admin.CreateOrganization())
			orgScoped := orgAdmin.Group("/:id", admin.requireOrgScope())
			{
				orgScoped.PUT("", admin.UpdateOrganization())
				orgScoped.DELETE("", admin.DeleteOrganization())
				orgScoped.GET("/members", admin.ListOrganizationMembers())
				orgScoped.POST("/members", admin.AddOrganizationMember())
				orgScoped.PUT("/members/:user_id", admin.UpdateOrganizationMember())
				orgScoped.DELETE("/members/:user_id", admin.RemoveOrganizationMember())
			}
		}

		// Notifier fans drift/failure alerts out to configured channels. Nil with a
		// nil DB (unit tests), or when no encryption key is configured — the drift
		// handler treats a nil notifier as a no-op. smtpCfg is held by reference
		// (not a value copy) so a runtime SMTP settings update via PUT
		// /notifications/smtp-config is observed by the Notifier on its next send
		// without recreating it — mirroring terraform-registry's notify.Mailer.
		// Any persisted config (saved via that same endpoint) is reloaded on top
		// of the YAML/env defaults so it survives a restart.
		//
		// identityTokenCipher (shared identity/crypto, used ONLY for
		// notification-channel targets) is separate from this repo's own
		// internal/crypto (used for CI-source tokens, OIDC secrets, etc.) — see
		// buildIdentityTokenCipher's doc comment. A nil egress guard applies the
		// shared identity/httpsafe strict default SSRF policy (this app has no
		// security.egress.allowlist equivalent config).
		var notifier *notify.Notifier
		var tokenCipher *identitycrypto.TokenCipher
		smtpCfg := &notify.SMTPConfig{
			Host:     cfg.Notifications.SMTP.Host,
			Port:     cfg.Notifications.SMTP.Port,
			From:     cfg.Notifications.SMTP.From,
			Username: cfg.Notifications.SMTP.Username,
			Password: cfg.Notifications.SMTP.Password,
			UseTLS:   cfg.Notifications.SMTP.UseTLS,
		}
		if database != nil {
			reloadNotificationsSMTPConfigFromDB(smtpCfg, settingsRepo)
			reloadNotificationsExpiryConfigFromDB(&cfg.Notifications, settingsRepo)
			if tc, err := buildIdentityTokenCipher(); err != nil {
				slog.Warn("notification channels disabled: channel-target encryption unavailable", "error", err)
			} else {
				tokenCipher = tc
				notifier = notify.New(repositories.NewNotificationChannelRepository(database), smtpCfg, tokenCipher, nil)
			}
		}

		// Operator-managed workflow-template store backs the /workflow endpoints;
		// nil with a nil DB (unit tests) so the handler falls back to the const.
		var templateRepo *repositories.WorkflowTemplateRepository
		if database != nil {
			templateRepo = repositories.NewWorkflowTemplateRepository(database)
		}

		// Phase 3 drift: CI pipeline connections + drift runs.
		drift := NewDriftHandlers(cfg, database, identityDB, notifier)
		p := v1.Group("/pipelines", requireAuth)
		{
			p.GET("", middleware.RequireScope(auth.ScopeSourcesManage), drift.ListPipelines())
			p.POST("", middleware.RequireScope(auth.ScopeSourcesManage), drift.CreatePipeline())
			p.PUT("/:id", middleware.RequireScope(auth.ScopeSourcesManage), drift.UpdatePipeline())
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
			cs.POST("/:id/verify", ciSources.VerifyCISource())
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
			d.GET("/workflow", middleware.RequireScope(auth.ScopeStateRead), drift.WorkflowTemplate(templateRepo))
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
		health := NewHealthHandlers(cfg, database, identityDB, notifier)
		hg := v1.Group("/health-lab", requireAuth)
		{
			hg.GET("/workflow", middleware.RequireScope(auth.ScopeStateRead), health.WorkflowTemplate(templateRepo))
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
		notif := NewNotificationHandlers(database, identityDB, notifier, tokenCipher)
		if database != nil {
			notif.WithSMTPSettings(settingsRepo, smtpCfg)
			notif.WithAPIKeyExpirySettings(&cfg.Notifications)
		}
		ng := v1.Group("/notifications", requireAuth, middleware.RequireScope(auth.ScopeAdmin))
		{
			ng.GET("/channels", notif.ListChannels())
			ng.POST("/channels", notif.CreateChannel())
			ng.PUT("/channels/:id", notif.UpdateChannel())
			ng.DELETE("/channels/:id", notif.DeleteChannel())
			ng.POST("/channels/:id/test", notif.TestChannel())
			ng.GET("/smtp-config", notif.GetSMTPConfig())
			ng.PUT("/smtp-config", notif.PutSMTPConfig())
			ng.POST("/test-email", notif.TestEmail())
			ng.GET("/api-key-expiry", notif.GetAPIKeyExpiryConfig())
			ng.PUT("/api-key-expiry", notif.PutAPIKeyExpiryConfig())
		}

		// Background workers. Guarded on database so unit tests that build the
		// router with a nil DB don't spin up goroutines that would hit it. The
		// syncer OBJECT is always attached (post-write refreshes and source-create
		// backfills must work on every replica); the PERIODIC loops — schedule
		// runner + statesync reconcile — run only on worker-enabled replicas, and
		// among those a Postgres advisory lock elects exactly ONE leader, so a
		// mis-scaled deployment with several worker-enabled replicas can no
		// longer double-fire schedules, syncs, or expiry emails. Non-leaders
		// stand by and promote automatically when the leader's session dies.
		if database != nil {
			syncer := statesync.New(
				repositories.NewSourceRepository(database),
				repositories.NewStateAnalysisRepository(database),
				ConnectSource,
			)
			sources.AttachSyncer(syncer)
			if cfg.Workers.Enabled {
				startWorkers := func() (stopWorkers func()) {
					runner := scheduler.New(repositories.NewScheduleRepository(database), driftDisp)
					runner.Start()
					syncer.Start()
					// Reap drift runs stuck in "dispatched" whose CI job never called
					// back (build/agent died), firing the same failure alert a real
					// callback would.
					reconciler := driftreconcile.New(
						repositories.NewDriftRepository(database),
						driftFailureNotifier{drift: drift},
						cfg.Drift.RunTTL, cfg.Drift.ReconcileInterval,
					)
					reconciler.Start()
					// Same backstop for version-lab health runs, which carry the
					// identical stuck-dispatched failure mode in a separate table. It
					// reuses the drift TTL/interval (the anchor and reasoning are the
					// same: created_at, a single end-of-job callback, no heartbeat).
					healthReconciler := healthreconcile.New(
						repositories.NewHealthRepository(database),
						healthFailureNotifier{health: health},
						cfg.Drift.RunTTL, cfg.Drift.ReconcileInterval,
					)
					healthReconciler.Start()

					// API key expiry notifier: periodic per-user warning emails for
					// keys nearing expiry. Leader-gated like the jobs above so a
					// multi-replica deployment cannot double-send warning emails.
					expiryNotifier := identitynotify.NewAPIKeyExpiryNotifier(
						idstore.NewAPIKeyRepository(identityDB),
						idstore.NewUserRepository(identityDB),
						func() identitynotify.ExpiryConfig {
							return identitynotify.ExpiryConfig{
								Enabled:        cfg.Notifications.Enabled,
								APIKeyExpiring: cfg.Notifications.Events.APIKeyExpiring,
								SMTP: identitymailer.Config{
									Host:     smtpCfg.Host,
									Port:     smtpCfg.Port,
									From:     smtpCfg.From,
									Username: smtpCfg.Username,
									Password: smtpCfg.Password,
									UseTLS:   smtpCfg.UseTLS,
								},
								WarningDays:        cfg.Notifications.APIKeyExpiryWarningDays,
								CheckIntervalHours: cfg.Notifications.APIKeyExpiryCheckIntervalHours,
							}
						},
						identitynotify.ExpiryOptions{ProductName: "Terraform State Manager"},
					)
					go func() { _ = expiryNotifier.Start(context.Background()) }()

					return func() {
						runner.Stop()
						syncer.Stop()
						reconciler.Stop()
						healthReconciler.Stop()
						_ = expiryNotifier.Stop()
					}
				}
				elector := leaderelect.New(database, startWorkers)
				elector.Start()
				stop = elector.Stop
			} else {
				slog.Info("background workers disabled on this replica (workers.enabled=false); " +
					"schedule firing and periodic state sync run on a worker-enabled replica")
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
		// NewDiscoveryClient (terraform-suite-identity v0.17.0+) fails closed for
		// a plaintext "http://" sibling URL, protecting the unauthenticated
		// manifest fetch from tampering by a network-position attacker. Treat
		// that failure the same as an absent/unreachable sibling: log it and run
		// standalone rather than starting an insecure client implicitly.
		dc, err := suite.NewDiscoveryClient(cfg.Suite.SiblingURL, buildSuiteManifest(cfg), cfg.Suite.PollInterval)
		if err != nil {
			slog.Error("suite discovery: failed to start sibling discovery client", "sibling_url", cfg.Suite.SiblingURL, "error", err)
		} else {
			ctx, cancel := context.WithCancel(context.Background())
			go dc.Start(ctx)
			suiteClient = dc
			prevStop := stop
			stop = func() { cancel(); prevStop() }
		}
	}

	return r, stop, nil
}

// reloadNotificationsSMTPConfigFromDB applies any persisted SMTP relay
// configuration (saved via PUT /notifications/smtp-config) onto smtp, so a
// runtime admin change survives a process restart. Fields are set in place
// (never reassigned) so the Notifier holding this same pointer observes the
// reloaded values. Mirrors terraform-registry's reloadNotificationsConfigFromDB.
func reloadNotificationsSMTPConfigFromDB(smtp *notify.SMTPConfig, settingsRepo *repositories.SystemSettingsRepository) {
	raw, err := settingsRepo.GetNotificationsConfig(context.Background())
	if err != nil || raw == nil {
		return
	}
	var dbc notificationsSMTPConfigDB
	if err := json.Unmarshal(raw, &dbc); err != nil {
		slog.Error("notifications startup: failed to parse persisted smtp config", "error", err)
		return
	}
	smtp.Host = dbc.SMTP.Host
	smtp.Port = dbc.SMTP.Port
	smtp.Username = dbc.SMTP.Username
	smtp.From = dbc.SMTP.From
	smtp.UseTLS = dbc.SMTP.UseTLS
	if dbc.SMTP.PasswordEncrypted != "" {
		if pw, derr := crypto.Decrypt([]byte(dbc.SMTP.PasswordEncrypted)); derr != nil {
			slog.Error("notifications startup: failed to decrypt persisted smtp password", "error", derr)
		} else {
			smtp.Password = string(pw)
		}
	}
}

// reloadNotificationsExpiryConfigFromDB applies any persisted API-key-expiry
// settings (saved via PUT /notifications/api-key-expiry) onto notifCfg, so a
// runtime admin change survives a process restart. A nil Expiry section
// (never explicitly saved via that endpoint) leaves the YAML/env defaults
// untouched -- mirrors reloadNotificationsSMTPConfigFromDB for the Expiry
// section of the same persisted blob.
func reloadNotificationsExpiryConfigFromDB(notifCfg *config.NotificationsConfig, settingsRepo *repositories.SystemSettingsRepository) {
	raw, err := settingsRepo.GetNotificationsConfig(context.Background())
	if err != nil || raw == nil {
		return
	}
	var dbc notificationsSMTPConfigDB
	if err := json.Unmarshal(raw, &dbc); err != nil {
		slog.Error("notifications startup: failed to parse persisted api key expiry config", "error", err)
		return
	}
	if dbc.Expiry == nil {
		return
	}
	notifCfg.Events.APIKeyExpiring = dbc.Expiry.APIKeyExpiring
	notifCfg.APIKeyExpiryWarningDays = dbc.Expiry.WarningDays
	notifCfg.APIKeyExpiryCheckIntervalHours = dbc.Expiry.CheckIntervalHours
}
