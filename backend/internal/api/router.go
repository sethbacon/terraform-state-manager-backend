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
	"time"

	"github.com/gin-gonic/gin"

	identitycrypto "github.com/sethbacon/terraform-suite-identity/identity/crypto"
	identitymailer "github.com/sethbacon/terraform-suite-identity/identity/mailer"
	idstore "github.com/sethbacon/terraform-suite-identity/identity/store"
	"github.com/sethbacon/terraform-suite-identity/identity/suite"

	"github.com/terraform-state-manager/terraform-state-manager/docs"
	"github.com/terraform-state-manager/terraform-state-manager/internal/api/scim"
	"github.com/terraform-state-manager/terraform-state-manager/internal/api/setup"
	"github.com/terraform-state-manager/terraform-state-manager/internal/approles"
	"github.com/terraform-state-manager/terraform-state-manager/internal/auth"
	"github.com/terraform-state-manager/terraform-state-manager/internal/auth/mtls"
	"github.com/terraform-state-manager/terraform-state-manager/internal/config"
	"github.com/terraform-state-manager/terraform-state-manager/internal/credlifecycle"
	"github.com/terraform-state-manager/terraform-state-manager/internal/crypto"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
	"github.com/terraform-state-manager/terraform-state-manager/internal/egress"
	"github.com/terraform-state-manager/terraform-state-manager/internal/legalhold"
	"github.com/terraform-state-manager/terraform-state-manager/internal/middleware"
	"github.com/terraform-state-manager/terraform-state-manager/internal/platformadmin"
	"github.com/terraform-state-manager/terraform-state-manager/internal/services/notify"
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

	// The platform-admin carrier: TSM's own answer to "who administers this
	// deployment", on the APP connection with its audit outbox beside it, and
	// resolved against the identity connection where principals live.
	//
	// Verified here rather than assumed. The three tables it addresses are all
	// unqualified and placed by their connection's search_path, so a startup line
	// naming where each one actually resolved is the difference between finding a
	// mis-routed carrier now and discovering it as an empty administrator list.
	// Both connections are nil in the unit-test rig, where there is no carrier and
	// therefore no elevation.
	//
	// CONSTRUCTED HERE, ABOVE THE mTLS REGISTRATION, because that middleware
	// now resolves a certificate's `admin` against it (#476). Registering the
	// middleware later instead would have been the smaller diff and the wrong
	// one: /health, /ready and the two swagger routes register before this
	// point, and gin binds the global chain at registration time, so they
	// would have silently stopped seeing mTLS at all.
	var platformAdmins *platformadmin.Service
	if database != nil && identityDB != nil {
		// Its own error variable: this block moved above the point where the
		// function's shared `err` is declared, and reusing that one only worked
		// by accident of ordering.
		svc, cerr := platformadmin.New(database, identityDB)
		if cerr != nil {
			return nil, stop, fmt.Errorf("failed to build the platform-admin carrier: %w", cerr)
		}
		if verr := svc.Verify(context.Background()); verr != nil {
			return nil, stop, fmt.Errorf("platform-admin carrier is not usable: %w", verr)
		}
		platformAdmins = svc
	}

	// mTLS: when enabled, a verified client cert (against the configured client CA)
	// authenticates the request additively, before JWT auth. No-op if not enabled
	// or if the TLS layer didn't verify a client cert.
	if cfg.Auth.MTLS.Enabled {
		mtlsProvider, err := mtls.NewProvider(cfg.Auth.MTLS)
		if err != nil {
			return nil, stop, fmt.Errorf("failed to initialise mTLS provider: %w", err)
		}
		r.Use(mtls.AuthMiddleware(mtlsProvider, platformAdmins))
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

	// One sweeper, shared by every route that can reduce a principal's derived
	// authority (#330). The revoke-all watermark lives on the app connection so
	// it works whether identity data shares the app database (the default) or
	// lives in its own (TSM_IDENTITY_DATABASE_*); the API-key and organization
	// repositories live on the identity connection that owns those tables.
	userRevocationRepo := repositories.NewUserTokenRevocationRepository(database)
	// One Members, shared by the credential sweeper and by the per-request tenant
	// scope resolution below. Two instances would be two answers to "which
	// organizations grant this principal this scope", derived from the same
	// tables through the same role source — and the copy that is wrong is the one
	// nobody is looking at.
	orgMembers := approles.NewMembers(identityDB, database, approles.RoleSource(cfg.Authz.RoleSource))
	credSweeper := credlifecycle.NewSweeper(
		userRevocationRepo,
		idstore.NewAPIKeyRepository(identityDB),
		orgMembers,
		platformAdmins,
	)

	authHandlers, err := NewAuthHandlers(cfg, identityDB, database, WithAuthCredentialSweeper(credSweeper))
	if err != nil {
		return nil, stop, err
	}

	// Load any DB-stored OIDC config (written by the setup wizard) into the live
	// auth handler, so OIDC configured via the wizard survives a restart without
	// needing config-file settings. Takes precedence over an empty config.
	oidcConfigRepo := repositories.NewOIDCConfigRepository(database)
	if database != nil {
		if active, oerr := oidcConfigRepo.GetActiveOIDCConfig(context.Background()); oerr == nil && active != nil {
			if secret, derr := crypto.DecryptFor(active.ClientSecretEncrypted, crypto.PurposeOIDCClientSecret); derr != nil {
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

	requireAuth := middleware.AuthMiddleware(authHandlers.UserRepo(), authHandlers.TokenRepo(), authHandlers.APIKeyRepo(), authHandlers.OrgRepo(), userRevocationRepo, platformAdmins)
	optionalAuth := middleware.OptionalAuthMiddleware(authHandlers.UserRepo(), authHandlers.TokenRepo(), userRevocationRepo, platformAdmins)

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
		setupHandlers := setup.NewHandlers(settingsRepo, oidcConfigRepo, repositories.NewSourceRepository(database), identityDB, database, platformAdmins, cfg, authHandlers.SetOIDCProvider)
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
			a.POST("/ldap/login", middleware.LoginRateLimit(), authHandlers.LDAPLoginHandler())
			// SAML 2.0 SP endpoints: SP metadata and the Assertion Consumer Service.
			a.GET("/saml/metadata", authHandlers.SAMLMetadataHandler())
			a.POST("/saml/acs", authHandlers.SAMLACSHandler())
			// Authenticated session endpoints.
			a.GET("/me", requireAuth, authHandlers.MeHandler())
			a.POST("/refresh", requireAuth, authHandlers.RefreshHandler())
			// Logout is POST-only (#274). v1 mounts CSRFProtect, which skips
			// safe methods, so a GET logout sat outside the double-submit check
			// and a cross-site link could force a victim's session to be
			// revoked. There is deliberately no GET route: re-adding one
			// reopens the forced-logout vector.
			a.POST("/logout", optionalAuth, authHandlers.LogoutPostHandler())
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
		apiKeys := NewAPIKeysHandlers(identityDB, database, approles.RoleSource(cfg.Authz.RoleSource))
		// A TenantScope for the key routes, resolved on state:read.
		//
		// THIS IS AN APPROXIMATION AND IT IS THE CONSERVATIVE ONE. The /apikeys
		// group carries no RequireScope: minting is self-service, and a key's
		// privileges are capped at authentication by its owner's live scopes, so
		// the question here is only "which organizations does this caller belong
		// to" -- which Resolve cannot answer without being given a verb.
		// state:read is the weakest assignable scope and the one the viewer role
		// template grants, so resolving on it approximates plain membership.
		//
		// Where the approximation fails it fails CLOSED: a member who somehow
		// holds only a non-read scope in an organization cannot mint a key there,
		// and gets a refusal rather than a key in the wrong tenant. The real
		// authority is mintKey's CheckMembership on the OWNER; this only bounds
		// which organization the caller may name.
		tenantScopeAPIKeys := middleware.TenantScope(orgMembers, platformAdmins, auth.ScopeStateRead)
		ak := v1.Group("/apikeys", requireAuth)
		{
			ak.GET("", apiKeys.ListAPIKeys())
			ak.POST("", tenantScopeAPIKeys, apiKeys.CreateAPIKey())
			ak.GET("/:id", apiKeys.GetAPIKey())
			ak.PUT("/:id", apiKeys.UpdateAPIKey())
			ak.DELETE("/:id", apiKeys.DeleteAPIKey())
			ak.POST("/:id/rotate", tenantScopeAPIKeys, apiKeys.RotateAPIKey())
		}

		// Phase 1 read plane: state sources + analysis.
		sources := NewSourcesHandlers(database, identityDB)
		// #393 Phase 2b: measure the organization-scoped reads against the
		// unscoped ones this route family still serves. Off by default; changes
		// nothing that is returned.
		// The existence check for an acting organization (#436). orgMembers is
		// this deployment's ONE construction of the shared organization
		// repository — internal/approles' guard test refuses a second one — and
		// its reads are the library's, promoted unchanged.
		sources.AttachOrganizations(orgMembers)
		// The tenant scope for the /sources READ routes.
		//
		// platformAdmins is a *platformadmin.Service and may be NIL here (the
		// unit-test rig has no connections). That is deliberately safe rather
		// than merely tolerated: a nil *Service still satisfies the interface,
		// its IsPlatformAdmin returns platformadmin.ErrNotConfigured, and
		// tenantscope.Resolve treats an absent carrier the way middleware.elevate
		// does — as authority WITHHELD, not granted. A `platformAdmins != nil`
		// guard here would type-assert its way to the same place and hide that.
		//
		// Registered PER ROUTE and carrying the same auth.Scope as the route's
		// own RequireScope, not once on the group: a scope resolved for
		// state:read says nothing about who may WRITE in those organizations,
		// and handing it to the sources:manage routes would, in Phase 3,
		// authorize an edit in an organization the caller may only read. See
		// middleware.TenantScope.
		tenantScopeStateRead := middleware.TenantScope(orgMembers, platformAdmins, auth.ScopeStateRead)
		// A SECOND instance, for sources:manage, and it must not be the one above.
		//
		// A scope resolved for state:read answers "which organizations may this
		// caller READ in". Handing that to a route that CREATES would let a
		// caller stamp a new source into an organization they may only read —
		// the precise widening middleware.TenantScope's own doc warns about.
		//
		// auth.ReadWritePairs() pairs only state:read->state:write and
		// organizations:read->organizations:write, so sources:manage is a literal
		// membership match with no write-implies-read widening: this resolves
		// exactly the organizations whose role template grants sources:manage.
		tenantScopeSourcesManage := middleware.TenantScope(orgMembers, platformAdmins, auth.ScopeSourcesManage)
		// A THIRD and FOURTH instance, for the two routes that write a partition
		// root of their own rather than inheriting one (#436).
		//
		// state:execute dispatches a health run, and state:transfer moves a state
		// file between two sources. Each stamps the row with the caller's ACTING
		// organization, so each needs the scope resolved for the verb it is about
		// to perform -- reusing tenantScopeStateRead here would let a caller who
		// may only READ in an organization dispatch work into it, and reusing
		// tenantScopeSourcesManage would answer a question about managing sources
		// that neither route is asking.
		tenantScopeStateExecute := middleware.TenantScope(orgMembers, platformAdmins, auth.ScopeStateExecute)
		tenantScopeStateTransfer := middleware.TenantScope(orgMembers, platformAdmins, auth.ScopeStateTransfer)
		// state:drift, for the route that dispatches a drift run. Its handler
		// resolves an acting organization and therefore 500s without a scope —
		// so this was not a hardening gap but a BROKEN route: every dispatch
		// returned "the tenant scope was not resolved". Found by
		// TestEveryScopeAwareHandlerIsRoutedWithTenantScope.
		tenantScopeStateDrift := middleware.TenantScope(orgMembers, platformAdmins, auth.ScopeStateDrift)
		// state:write, for the edit plane. Its routes reach their source through
		// sourceAndConnector, which now refuses a source the caller's
		// organization does not own -- and cannot, without a scope to check.
		tenantScopeStateWrite := middleware.TenantScope(orgMembers, platformAdmins, auth.ScopeStateWrite)
		// Bound state-operation requests so a hung or slow backend cannot block the
		// handler goroutine (and any per-key lock it holds) indefinitely (#263).
		s := v1.Group("/sources", requireAuth, middleware.RequestTimeout(5*time.Minute))
		{
			s.GET("", middleware.RequireScope(auth.ScopeStateRead), tenantScopeStateRead, sources.ListSources())
			s.POST("", middleware.RequireScope(auth.ScopeSourcesManage), tenantScopeSourcesManage, sources.CreateSource())
			// Static /test must not collide with /:id below: gin resolves static
			// segments before params, so POST /sources/test is unambiguous.
			s.POST("/test", middleware.RequireScope(auth.ScopeSourcesManage), tenantScopeSourcesManage, sources.TestSourceConfig())
			s.GET("/:id", middleware.RequireScope(auth.ScopeStateRead), tenantScopeStateRead, sources.GetSource())
			// tenantScopeSourcesManage on BOTH, because both MUTATE. An update
			// rewrites a source's connector config and stored credentials; a
			// delete cascades to eight dependent tables. Neither had a scope
			// resolved, so neither could check ownership.
			s.PUT("/:id", middleware.RequireScope(auth.ScopeSourcesManage), tenantScopeSourcesManage, sources.UpdateSource())
			s.DELETE("/:id", middleware.RequireScope(auth.ScopeSourcesManage), tenantScopeSourcesManage, sources.DeleteSource())
			s.POST("/:id/test", middleware.RequireScope(auth.ScopeStateRead), tenantScopeStateRead, sources.TestSource())
			s.GET("/:id/states", middleware.RequireScope(auth.ScopeStateRead), tenantScopeStateRead, sources.ListStates())
			s.GET("/:id/state/analysis", middleware.RequireScope(auth.ScopeStateRead), tenantScopeStateRead, sources.AnalyzeState())
			s.GET("/:id/state/raw", middleware.RequireScope(auth.ScopeStateRead), tenantScopeStateRead, sources.RawState())
			s.GET("/:id/state/resources", middleware.RequireScope(auth.ScopeStateRead), tenantScopeStateRead, sources.ListStateResources())
			s.GET("/:id/state/outputs", middleware.RequireScope(auth.ScopeStateRead), tenantScopeStateRead, sources.StateOutputs())
			s.GET("/:id/state/history", middleware.RequireScope(auth.ScopeStateRead), tenantScopeStateRead, sources.StateHistory())
			s.GET("/:id/state/report", middleware.RequireScope(auth.ScopeStateRead), tenantScopeStateRead, sources.StateReport())
			s.GET("/:id/modules", middleware.RequireScope(auth.ScopeStateRead), tenantScopeStateRead, sources.ListStateModules())
			// Freshness: locked module versions vs the sibling registry's latest.
			// Inert when standalone (no active sibling -> every module "no_registry").
			s.GET("/:id/modules/freshness", middleware.RequireScope(auth.ScopeStateRead), tenantScopeStateRead, sources.ListStateModuleFreshness(func() *suite.DiscoveryClient { return suiteClient }))

			// Phase 2 edit plane (validate → backup → write → audit; restore).
			s.PUT("/:id/state/raw", middleware.RequireScope(auth.ScopeStateWrite), tenantScopeStateWrite, sources.EditState())
			s.POST("/:id/state/diff", middleware.RequireScope(auth.ScopeStateWrite), tenantScopeStateWrite, sources.EditDiff())
			s.POST("/:id/state/operations", middleware.RequireScope(auth.ScopeStateWrite), tenantScopeStateWrite, sources.StateOperation())
			s.GET("/:id/state/backups", middleware.RequireScope(auth.ScopeStateRead), tenantScopeStateRead, sources.ListBackups())
			s.GET("/:id/state/backups/:backupId", middleware.RequireScope(auth.ScopeStateRead), tenantScopeStateRead, sources.GetBackupContent())
			s.GET("/:id/state/backups/:backupId/diff", middleware.RequireScope(auth.ScopeStateRead), tenantScopeStateRead, sources.DiffBackup())
			s.POST("/:id/state/backups/:backupId/restore", middleware.RequireScope(auth.ScopeStateWrite), tenantScopeStateWrite, sources.RestoreBackup())
			s.GET("/:id/state/locks", middleware.RequireScope(auth.ScopeStateRead), tenantScopeStateRead, sources.ListLocks())
			s.DELETE("/:id/state/lock", middleware.RequireScope(auth.ScopeAdmin), sources.ForceUnlock())

			// Phase 2 transfer plane: cross-source backup (copy) and migrate (move).
			s.POST("/:id/state/backup", middleware.RequireScope(auth.ScopeStateTransfer), tenantScopeStateTransfer, sources.BackupToSource())
			s.POST("/:id/state/migrate", middleware.RequireScope(auth.ScopeStateTransfer), tenantScopeStateTransfer, sources.MigrateToSource())
		}

		v1.GET("/transfers/:id", requireAuth, middleware.RequireScope(auth.ScopeStateRead), sources.GetTransfer())

		// Cross-app: states consuming a given registry module (a sibling registry
		// server-proxies to this to power its "Consumed by" panel).
		v1.GET("/consumers", middleware.RequireSuiteServiceToken(cfg.Suite.ServiceToken), sources.Consumers())

		// Cross-app: a sibling app federates its audit entries here (shared-store
		// only — enforced in the handler, and advertised via audit.ingest.v1).
		auditIngest := NewAuditIngestHandlers(identityDB, database, cfg)
		v1.POST("/audit/ingest", middleware.RequireSuiteServiceToken(cfg.Suite.ServiceToken), auditIngest.Ingest())

		// Home dashboard: cross-source aggregated overview.
		// The dashboard and report routes resolve a scope now (#459), so they
		// need the middleware that publishes one. tenantScopeStateRead is the
		// correct instance and not merely a convenient one: these routes READ,
		// and a scope resolved for sources:manage would answer "which
		// organizations may this caller WRITE in", which is a different and
		// narrower question that would hide rows they are entitled to see.
		v1.GET("/dashboard/overview", requireAuth, middleware.RequireScope(auth.ScopeStateRead), tenantScopeStateRead, sources.DashboardOverview())
		v1.GET("/dashboard/states-by-version", requireAuth, middleware.RequireScope(auth.ScopeStateRead), tenantScopeStateRead, sources.StatesByVersion())

		// State-store reconcile. A POST (CSRF-protected) so the state-changing
		// re-read cannot be triggered by a replayable cross-site GET (#215); the
		// dashboard/report GETs stay pure reads that serve the current store.
		v1.POST("/reconcile", requireAuth, middleware.RequireScope(auth.ScopeStateRead), sources.ReconcileSources())

		// Reports: cross-fleet state-file query, preview, and multi-format export.
		v1.GET("/reports/states", requireAuth, middleware.RequireScope(auth.ScopeStateRead), tenantScopeStateRead, sources.ReportStates())
		v1.GET("/reports/states/export", requireAuth, middleware.RequireScope(auth.ScopeStateRead), tenantScopeStateRead, sources.ReportStatesExport())

		// Identity management (admin scope): users, organizations, roles, audit log.
		admin := NewAdminHandlers(identityDB, database, approles.RoleSource(cfg.Authz.RoleSource), WithAdminCredentialSweeper(credSweeper), WithPlatformAdmins(platformAdmins))
		ag := v1.Group("/admin", requireAuth, middleware.RequireScope(auth.ScopeAdmin))
		{
			ag.GET("/stats", admin.Stats())

			// Audit legal holds (#373).
			//
			// Built on identityDB, NOT the app pool. audit_logs is reached
			// through the identity connection and so is the retention sweep's
			// exemption -- a hold written on a different pool than the sweep
			// reads is invisible to it, and every hold would look placed while
			// the sweep deleted the rows anyway. Passing the same handle makes
			// them the same connection by construction.
			//
			// A nil handle (the unit-test rig) leaves the routes registered and
			// answering 503, rather than absent: a route that vanishes with the
			// database is one whose authorization nothing ever checks.
			holdRepo, holdErr := legalhold.New(identityDB)
			if holdErr != nil {
				slog.Warn("legal holds unavailable", "error", holdErr)
			}
			holds := NewLegalHoldHandlers(holdRepo, idstore.NewAuditRepository(identityDB))
			ag.GET("/legal-holds", holds.ListHolds())
			ag.POST("/legal-holds", holds.PlaceHold())
			ag.POST("/legal-holds/:id/release", holds.ReleaseHold())

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

			// The platform-admin carrier: who administers THIS deployment.
			//
			// Gated on the same flat admin scope as the rest of this group, and
			// that is what makes an upgrade survivable: a deployment whose
			// administrators hold `admin` through a role template today can
			// populate its own carrier from here, without the chicken-and-egg
			// that a carrier-gated management API would have. The first-run
			// wizard (internal/api/setup) is the path for a deployment that has
			// no administrator at all.
			platformAdminHandlers := NewPlatformAdminHandlers(platformAdmins)
			ag.GET("/platform-admins", platformAdminHandlers.ListPlatformAdmins())
			ag.POST("/platform-admins", platformAdminHandlers.GrantPlatformAdmin())
			ag.DELETE("/platform-admins/:user_id", platformAdminHandlers.RevokePlatformAdmin())

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
		// buildIdentityTokenCipher's doc comment.
		//
		// The notifier takes the deployment's egress guard (internal/egress,
		// built from security.egress.allowlist at config.go's
		// security.egress.allowlist / TSM_SECURITY_EGRESS_ALLOWLIST and wired in
		// main.go). An empty allow-list makes that the strict default policy,
		// which is what the nil this used to pass meant — so a deployment that
		// configures nothing is unchanged, and one that allow-lists an internal
		// host gets that host for its webhooks too rather than having the setting
		// silently apply to some outbound paths and not others.
		var notifier *notify.Notifier
		var tokenCipher *identitycrypto.TokenCipher
		smtpCfg := &notify.SMTPConfig{
			Host:     cfg.Notifications.SMTP.Host,
			Port:     cfg.Notifications.SMTP.Port,
			From:     cfg.Notifications.SMTP.From,
			Username: cfg.Notifications.SMTP.Username,
			Password: cfg.Notifications.SMTP.Password,
			TLSMode:  identitymailer.TLSModeForUseTLS(cfg.Notifications.SMTP.UseTLS),
		}
		if database != nil {
			reloadNotificationsSMTPConfigFromDB(smtpCfg, settingsRepo)
			reloadNotificationsExpiryConfigFromDB(&cfg.Notifications, settingsRepo)
			if tc, err := buildIdentityTokenCipher(); err != nil {
				slog.Warn("notification channels disabled: channel-target encryption unavailable", "error", err)
			} else {
				tokenCipher = tc
				notifier = notify.New(repositories.NewNotificationChannelRepository(database), smtpCfg, tokenCipher, egress.Guard())
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
		drift.AttachOrganizations(orgMembers)
		p := v1.Group("/pipelines", requireAuth)
		{
			// The read routes resolve a tenant scope too (#393): listing CI
			// connections is organization-scoped, and the write routes verify
			// cross-references against it.
			p.GET("", middleware.RequireScope(auth.ScopeSourcesManage), tenantScopeSourcesManage, drift.ListPipelines())
			p.POST("", middleware.RequireScope(auth.ScopeSourcesManage), tenantScopeSourcesManage, drift.CreatePipeline())
			p.PUT("/:id", middleware.RequireScope(auth.ScopeSourcesManage), tenantScopeSourcesManage, drift.UpdatePipeline())
			p.DELETE("/:id", middleware.RequireScope(auth.ScopeSourcesManage), tenantScopeSourcesManage, drift.DeletePipeline())
			// Repo-setup wizard preflight: is the callback URL reachable from CI?
			p.GET("/callback-preflight", middleware.RequireScope(auth.ScopeSourcesManage), drift.CallbackPreflight())
		}

		// CI sources: org-level CI credentials + pipeline/repo/workflow discovery,
		// so pipeline connections can be created by selection.
		ciSources := NewCISourceHandlers(database, identityDB)
		ciSources.AttachOrganizations(orgMembers)
		cs := v1.Group("/ci-sources", requireAuth, middleware.RequireScope(auth.ScopeSourcesManage))
		{
			// EVERY route here resolves a tenant scope now (#393): the list and
			// each by-id route read through the scoped readers, and every by-id
			// route decrypts the source's shared credential on the way to a
			// provider call -- which is why the read routes are not exempt.
			// Wired per-route rather than on the group because the route class
			// guard reads registration lines.
			cs.GET("", tenantScopeSourcesManage, ciSources.ListCISources())
			cs.POST("", tenantScopeSourcesManage, ciSources.CreateCISource())
			cs.DELETE("/:id", tenantScopeSourcesManage, ciSources.DeleteCISource())
			cs.POST("/:id/verify", tenantScopeSourcesManage, ciSources.VerifyCISource())
			cs.GET("/:id/pipelines", tenantScopeSourcesManage, ciSources.ListSourcePipelines())
			cs.GET("/:id/repos", tenantScopeSourcesManage, ciSources.ListSourceRepos())
			cs.GET("/:id/repos/:repo/workflows", tenantScopeSourcesManage, ciSources.ListSourceWorkflows())
			// Repo-setup wizard: ADO service connections + pipeline creation.
			cs.GET("/:id/service-connections", tenantScopeSourcesManage, ciSources.ListSourceServiceConnections())
			cs.POST("/:id/repos/:repo/pipelines", tenantScopeSourcesManage, ciSources.CreateSourcePipeline())
			// Phase 2: commit the workflow via branch + PR, and poll the PR state.
			cs.POST("/:id/repos/:repo/workflow-setup", tenantScopeSourcesManage, ciSources.SetupSourceWorkflow())
			cs.GET("/:id/repos/:repo/prs/:pr", tenantScopeSourcesManage, ciSources.GetSourcePRState())
		}
		d := v1.Group("/drift", requireAuth)
		{
			d.GET("/workflow", middleware.RequireScope(auth.ScopeStateRead), drift.WorkflowTemplate(templateRepo))
			d.POST("/runs", middleware.RequireScope(auth.ScopeStateDrift), tenantScopeStateDrift, drift.CreateRun())
			// The run and record reads resolve a tenant scope too (#393): a drift
			// run carries a state key and a plan summary, and a drift record is
			// the acknowledgeable statement of what is wrong with a tenant's
			// infrastructure. The two acknowledge/resolve routes are the WRITE
			// side of the same root and resolve the scope for their own verb.
			d.GET("/runs", middleware.RequireScope(auth.ScopeStateRead), tenantScopeStateRead, drift.ListRuns())
			d.GET("/runs/:id", middleware.RequireScope(auth.ScopeStateRead), tenantScopeStateRead, drift.GetRun())

			// Drift records: the durable, acknowledgeable layer over runs, plus
			// push-style ingest for pipelines TSM did not dispatch.
			d.POST("/ingest", middleware.RequireScope(auth.ScopeStateDrift), tenantScopeStateDrift, drift.IngestDrift())
			d.GET("/records", middleware.RequireScope(auth.ScopeStateRead), tenantScopeStateRead, drift.ListDriftRecords())
			d.GET("/records/:id", middleware.RequireScope(auth.ScopeStateRead), tenantScopeStateRead, drift.GetDriftRecord())
			d.POST("/records/:id/acknowledge", middleware.RequireScope(auth.ScopeStateDrift), tenantScopeStateDrift, drift.AcknowledgeDriftRecord())
			d.POST("/records/:id/resolve", middleware.RequireScope(auth.ScopeStateDrift), tenantScopeStateDrift, drift.ResolveDriftRecord())
		}
		// Machine callback (authenticated by the per-run token, not a user
		// session), and DELIBERATELY WITHOUT middleware.TenantScope: a CI job
		// carries no principal, so there is no tenancy to resolve for it and the
		// middleware would refuse every legitimate callback. Its authority comes
		// from the credential instead -- the run the token authenticates names
		// the organization every statement afterwards runs under. See
		// callback_authority.go.
		v1.POST("/drift/runs/:id/results", drift.RunResults())

		// Phase 4 version lab: dispatch plan against pinned versions + health.
		health := NewHealthHandlers(cfg, database, identityDB, notifier)
		health.AttachOrganizations(orgMembers)
		hg := v1.Group("/health-lab", requireAuth)
		{
			hg.GET("/workflow", middleware.RequireScope(auth.ScopeStateRead), health.WorkflowTemplate(templateRepo))
			hg.POST("/runs", middleware.RequireScope(auth.ScopeStateExecute), tenantScopeStateExecute, health.CreateRun())
			hg.GET("/runs", middleware.RequireScope(auth.ScopeStateRead), tenantScopeStateRead, health.ListRuns())
			hg.GET("/runs/:id", middleware.RequireScope(auth.ScopeStateRead), tenantScopeStateRead, health.GetRun())
		}
		// The health callback, on the same terms as the drift one above.
		v1.POST("/health-lab/runs/:id/results", health.RunResults())

		// Scheduler: cron-driven schedules that dispatch drift runs. The same drift
		// dispatcher backs the HTTP "run now" endpoint and the background runner.
		driftDisp := driftDispatcher{drift: drift}
		scheduleHandlers := NewScheduleHandlers(database, identityDB, driftDisp)
		scheduleHandlers.AttachOrganizations(orgMembers)
		sg := v1.Group("/schedules", requireAuth)
		{
			// Every route in this group resolves a tenant scope, and each one
			// resolves it for the verb the route is guarded by: a scope resolved
			// for state:read handed to a route that dispatches would answer a
			// question about reading in order to permit an execution.
			sg.GET("", middleware.RequireScope(auth.ScopeStateRead), tenantScopeStateRead, scheduleHandlers.ListSchedules())
			sg.POST("", middleware.RequireScope(auth.ScopeSourcesManage), tenantScopeSourcesManage, scheduleHandlers.CreateSchedule())
			sg.GET("/:id", middleware.RequireScope(auth.ScopeStateRead), tenantScopeStateRead, scheduleHandlers.GetSchedule())
			sg.PUT("/:id", middleware.RequireScope(auth.ScopeSourcesManage), tenantScopeSourcesManage, scheduleHandlers.UpdateSchedule())
			sg.DELETE("/:id", middleware.RequireScope(auth.ScopeSourcesManage), tenantScopeSourcesManage, scheduleHandlers.DeleteSchedule())
			sg.POST("/:id/run", middleware.RequireScope(auth.ScopeSourcesManage), tenantScopeSourcesManage, scheduleHandlers.RunSchedule())
		}

		// Notification channels (admin): alert destinations + the drift-event hook.
		// Target URLs are secrets, so the whole group is admin-scoped.
		notif := NewNotificationHandlers(database, identityDB, notifier, tokenCipher)
		if database != nil {
			notif.WithSMTPSettings(settingsRepo, smtpCfg)
			notif.WithAPIKeyExpirySettings(&cfg.Notifications)
		}
		notif.AttachOrganizations(orgMembers)
		// A FIFTH TenantScope instance, for admin. The channel routes sit behind
		// auth.ScopeAdmin, so the scope resolved for them must be the admin one:
		// handing a scope resolved for a different verb to a route that CREATES is
		// the widening middleware.TenantScope's own doc warns about (#436).
		tenantScopeAdmin := middleware.TenantScope(orgMembers, platformAdmins, auth.ScopeAdmin)
		ng := v1.Group("/notifications", requireAuth, middleware.RequireScope(auth.ScopeAdmin))
		{
			ng.GET("/channels", notif.ListChannels())
			ng.POST("/channels", tenantScopeAdmin, notif.CreateChannel())
			ng.PUT("/channels/:id", notif.UpdateChannel())
			ng.DELETE("/channels/:id", notif.DeleteChannel())
			ng.POST("/channels/:id/test", notif.TestChannel())
			ng.GET("/smtp-config", notif.GetSMTPConfig())
			ng.PUT("/smtp-config", notif.PutSMTPConfig())
			ng.POST("/test-email", notif.TestEmail())
			ng.GET("/api-key-expiry", notif.GetAPIKeyExpiryConfig())
			ng.PUT("/api-key-expiry", notif.PutAPIKeyExpiryConfig())
		}

		// Background workers: the always-on state syncer plus the leader-elected
		// periodic loops (schedule runner, state-sync reconcile, drift/health
		// stuck-run reconcilers, API-key-expiry notifier). Extracted to
		// newBackgroundWorkers so this operationally-critical path is unit-testable
		// independent of NewRouter (#265); it returns a no-op stop when database is
		// nil (unit tests) or when workers are disabled on this replica.
		stop = newBackgroundWorkers(backgroundWorkerDeps{
			cfg:        cfg,
			database:   database,
			identityDB: identityDB,
			sources:    sources,
			drift:      drift,
			health:     health,
			driftDisp:  driftDisp,
			smtpCfg:    smtpCfg,
			auditRelay: platformAdmins.Relay(),
		})
	}

	// SCIM 2.0 provisioning (RFC 7644), mounted at the conventional top-level
	// /scim/v2 and only when enabled. Bearer-token auth + scim:provision scope
	// (admin satisfies it); no cookie auth, so it is outside the CSRF-protected
	// /api/v1 group. IdP clients present Authorization: Bearer <token>.
	if cfg.Auth.SCIM.Enabled {
		scimHandlers := scim.NewHandlers(cfg, identityDB, database, scim.WithCredentialSweeper(credSweeper))
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
		// manifest fetch from tampering by a network-position attacker. Since
		// v0.25.0 it also takes the deployment's egress guard, so a sibling on an
		// internal address must be allow-listed. Treat either failure the same as
		// an absent/unreachable sibling: log it and run standalone rather than
		// starting an unguarded client implicitly.
		dc, err := suite.NewDiscoveryClient(cfg.Suite.SiblingURL, buildSuiteManifest(cfg), cfg.Suite.PollInterval, egress.Guard())
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
	smtp.TLSMode = identitymailer.TLSModeForUseTLS(dbc.SMTP.UseTLS)
	// Resolved through the shared helper so this and the API agree on which
	// field holds the password, and on what an unreadable one means.
	if ct, legacy, ok := dbc.SMTP.decodeStoredPassword(); ok {
		pw, derr := crypto.DecryptFor(ct, crypto.PurposeSMTPRelayPassword)
		switch {
		case derr == nil:
			smtp.Password = string(pw)
		case legacy:
			// EXPECTED, and the message has to be actionable rather than
			// alarming. This value was written by the pre-fix path, which
			// stored the ciphertext as a Go string inside a JSON blob --
			// json.Marshal replaced every non-UTF-8 byte with U+FFFD, so the
			// password was destroyed as it was saved. It cannot be recovered
			// from anything; it has to be entered again.
			slog.Error("notifications startup: the stored SMTP password predates the encoding fix "+
				"and cannot be decrypted. It was corrupted when it was saved, not since, and no "+
				"key or backup will recover it. Re-enter it under Notifications > SMTP; sending "+
				"will authenticate without a password until you do",
				"error", derr)
		default:
			// A sealed value that will not open is a different problem: the
			// bytes survived, so this is a key mismatch or genuine corruption.
			slog.Error("notifications startup: failed to decrypt persisted smtp password. The stored "+
				"value is well-formed, so check TSM_ENCRYPTION_KEY (and TSM_ENCRYPTION_KEY_PREVIOUS "+
				"if you have rotated) before re-entering it",
				"error", derr)
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
