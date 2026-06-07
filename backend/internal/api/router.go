// Package api wires together all HTTP routes for the Terraform State Manager backend.
//
// Route grouping philosophy:
//   - Health, readiness, and version endpoints are unauthenticated and intended
//     for load balancers, orchestrators, and monitoring probes.
//   - Setup wizard endpoints (/api/v1/setup/) are gated by a one-time setup
//     token and permanently disabled after initial configuration completes.
//   - Auth endpoints (/api/v1/auth/) are publicly accessible but rate limited
//     to prevent brute-force attacks.
//   - All other admin and management endpoints (/api/v1/) require full
//     authentication, rate limiting, and audit logging.
package api

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"

	"github.com/terraform-state-manager/terraform-state-manager/internal/api/admin"
	alertsAPI "github.com/terraform-state-manager/terraform-state-manager/internal/api/alerts"
	"github.com/terraform-state-manager/terraform-state-manager/internal/api/analysis"
	backupsAPI "github.com/terraform-state-manager/terraform-state-manager/internal/api/backups"
	complianceAPI "github.com/terraform-state-manager/terraform-state-manager/internal/api/compliance"
	"github.com/terraform-state-manager/terraform-state-manager/internal/api/dashboards"
	migrationsAPI "github.com/terraform-state-manager/terraform-state-manager/internal/api/migrations"
	notificationsAPI "github.com/terraform-state-manager/terraform-state-manager/internal/api/notifications"
	"github.com/terraform-state-manager/terraform-state-manager/internal/api/reports"
	schedulerAPI "github.com/terraform-state-manager/terraform-state-manager/internal/api/scheduler"
	"github.com/terraform-state-manager/terraform-state-manager/internal/api/setup"
	"github.com/terraform-state-manager/terraform-state-manager/internal/api/snapshots"
	"github.com/terraform-state-manager/terraform-state-manager/internal/api/sources"
	webhooksAPI "github.com/terraform-state-manager/terraform-state-manager/internal/api/webhooks"
	"github.com/terraform-state-manager/terraform-state-manager/internal/auth"
	"github.com/terraform-state-manager/terraform-state-manager/internal/auth/oidc"
	"github.com/terraform-state-manager/terraform-state-manager/internal/config"
	"github.com/terraform-state-manager/terraform-state-manager/internal/crypto"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
	"github.com/terraform-state-manager/terraform-state-manager/internal/middleware"
	"github.com/terraform-state-manager/terraform-state-manager/internal/services"
	"github.com/terraform-state-manager/terraform-state-manager/internal/services/backup"
	complianceSvc "github.com/terraform-state-manager/terraform-state-manager/internal/services/compliance"
	"github.com/terraform-state-manager/terraform-state-manager/internal/services/migration"
	"github.com/terraform-state-manager/terraform-state-manager/internal/services/notification"
	schedulerSvc "github.com/terraform-state-manager/terraform-state-manager/internal/services/scheduler"
	snapshotSvc "github.com/terraform-state-manager/terraform-state-manager/internal/services/snapshot"
	"github.com/terraform-state-manager/terraform-state-manager/internal/storage"

	docs "github.com/terraform-state-manager/terraform-state-manager/docs"
)

// BackgroundServices holds references to background resources that must be
// stopped during graceful shutdown. The caller (cmd/server) is responsible for
// calling Shutdown() when the process receives a termination signal.
type BackgroundServices struct {
	rateLimiters []*middleware.RateLimiter
	scheduler    *schedulerSvc.Scheduler
}

// Shutdown stops all background goroutines. It should be called after the HTTP
// server has been shut down so that in-flight requests are drained first.
func (bg *BackgroundServices) Shutdown() {
	slog.Info("stopping background services")
	if bg.scheduler != nil {
		bg.scheduler.Stop()
	}
	for _, rl := range bg.rateLimiters {
		rl.Stop()
	}
	slog.Info("all background services stopped")
}

// NewRouter creates and configures the Gin router with all TSM API routes.
// identityDB backs identity data access (users, organizations, API keys, OIDC
// config, audit logs, role templates); it equals db unless the identity-schema
// cutover is enabled, in which case it targets the shared identity schema.
func NewRouter(cfg *config.Config, db, identityDB *sql.DB) (*gin.Engine, *BackgroundServices) {
	router := gin.New()

	// Identity repositories use identityDB so they follow the configured
	// identity schema; feature repositories below stay on db (public schema).
	userRepo := repositories.NewUserRepository(identityDB)
	apiKeyRepo := repositories.NewAPIKeyRepository(identityDB)
	auditRepo := repositories.NewAuditRepository(identityDB)
	orgRepo := repositories.NewOrganizationRepository(identityDB)
	roleTemplateRepo := repositories.NewRoleTemplateRepository(identityDB)

	// Wrap the identity connection with sqlx for repositories that require it.
	sqlxDB := sqlx.NewDb(identityDB, "postgres")
	oidcConfigRepo := repositories.NewOIDCConfigRepository(sqlxDB)

	// Phase 1 repositories
	sourceRepo := repositories.NewStateSourceRepository(db)
	analysisRunRepo := repositories.NewAnalysisRunRepository(db)
	analysisResultRepo := repositories.NewAnalysisResultRepository(db)

	// Get encryption key from environment for token encryption
	encryptionKey := os.Getenv("ENCRYPTION_KEY")
	if encryptionKey == "" {
		log.Fatal("ENCRYPTION_KEY environment variable must be set")
	}

	// Initialize token cipher for encrypting sensitive values at rest
	tokenCipher, err := crypto.NewTokenCipher([]byte(encryptionKey))
	if err != nil {
		log.Fatalf("Failed to initialize token cipher: %v", err)
	}

	// Global middleware
	router.Use(gin.Recovery())
	router.Use(middleware.RequestIDMiddleware())
	router.Use(middleware.MetricsMiddleware())
	router.Use(LoggerMiddleware(cfg))
	router.Use(CORSMiddleware(cfg))
	router.Use(middleware.SecurityHeadersMiddleware(middleware.APISecurityHeadersConfig()))

	// Health, readiness, and version endpoints (unauthenticated)
	router.GET("/health", healthCheckHandler(db))
	router.GET("/ready", readinessHandler(db))
	router.GET("/version", versionHandler())

	// Serve OpenAPI spec (unauthenticated)
	router.GET("/swagger.yaml", func(c *gin.Context) {
		c.Data(http.StatusOK, "application/yaml; charset=utf-8", docs.SwaggerYAML)
	})
	router.GET("/swagger.json", func(c *gin.Context) {
		c.Data(http.StatusOK, "application/json; charset=utf-8", docs.SwaggerJSON)
	})

	// Initialize auth handlers
	authHandlers, err := admin.NewAuthHandlers(cfg, identityDB, oidcConfigRepo)
	if err != nil {
		log.Fatalf("Failed to initialize auth handlers: %v", err)
	}

	// Load OIDC configuration from database if available (setup wizard stores config in DB).
	// This takes precedence over static config file settings and allows OIDC to work
	// without requiring config.yaml to have OIDC settings pre-configured.
	if activeOIDCCfg, oidcErr := oidcConfigRepo.GetActiveOIDCConfig(context.Background()); oidcErr == nil && activeOIDCCfg != nil {
		// Decrypt the client secret. An env-managed placeholder (used by dev/OIDC
		// stacks that configure the secret via TSM_AUTH_OIDC_* env vars) is not a
		// real ciphertext, so treat it as "use static config" rather than logging a
		// spurious decrypt error.
		clientSecret, decErr := tokenCipher.Open(activeOIDCCfg.ClientSecretEncrypted)
		isEnvManaged := activeOIDCCfg.ClientSecretEncrypted == "" || activeOIDCCfg.ClientSecretEncrypted == "env-var-managed"
		if isEnvManaged {
			slog.Info("OIDC client secret is env-managed; using static OIDC configuration", "issuer", activeOIDCCfg.IssuerURL)
		} else if decErr != nil {
			slog.Error("Failed to decrypt OIDC client secret from database", "error", decErr)
		} else {
			liveCfg := &config.OIDCConfig{
				Enabled:      true,
				IssuerURL:    activeOIDCCfg.IssuerURL,
				ClientID:     activeOIDCCfg.ClientID,
				ClientSecret: clientSecret,
				RedirectURL:  activeOIDCCfg.RedirectURL,
				Scopes:       activeOIDCCfg.GetScopes(),
			}
			provider, provErr := oidc.NewOIDCProvider(liveCfg)
			if provErr != nil {
				slog.Error("Failed to initialize OIDC provider from database config", "error", provErr, "issuer", activeOIDCCfg.IssuerURL)
			} else {
				authHandlers.SetOIDCProvider(provider)
				slog.Info("OIDC provider loaded from database configuration", "issuer", activeOIDCCfg.IssuerURL)

			}
		}
	}

	// Initialize admin handlers. Identity-backed handlers use identityDB; stats
	// uses the identity sqlx wrapper (identity entities resolve to the identity
	// schema, feature counts fall back to public via search_path).
	apiKeyHandlers := admin.NewAPIKeyHandlers(cfg, identityDB)
	userHandlers := admin.NewUserHandlers(cfg, identityDB)
	orgHandlers := admin.NewOrganizationHandlers(cfg, identityDB)
	statsHandlers := admin.NewStatsHandler(sqlxDB)
	roleTemplateHandlers := admin.NewRoleHandlers(roleTemplateRepo)
	oidcAdminHandlers := admin.NewOIDCConfigAdminHandlers(oidcConfigRepo)
	auditLogHandlers := admin.NewAuditLogHandlers(identityDB)
	gdprHandlers := admin.NewGDPRHandlers(services.NewUserService(identityDB))

	// Initialize setup wizard handlers
	setupHandlers := setup.NewHandlers(
		cfg, identityDB, tokenCipher, oidcConfigRepo, userRepo, orgRepo, authHandlers.SetOIDCProvider,
	)

	// Initialize Phase 1 handlers
	sourceHandlers := sources.NewHandlers(cfg, sourceRepo, tokenCipher)
	analysisHandlers := analysis.NewHandlers(cfg, analysisRunRepo, analysisResultRepo, sourceRepo, tokenCipher)

	// Phase 2 repositories
	reportRepo := repositories.NewReportRepository(db)

	// Phase 3 repositories
	scheduledTaskRepo := repositories.NewScheduledTaskRepository(db)
	snapshotRepo := repositories.NewStateSnapshotRepository(db)
	driftRepo := repositories.NewDriftEventRepository(db)

	// Phase 4 repositories
	backupRepo := repositories.NewBackupRepository(db)
	retentionPolicyRepo := repositories.NewRetentionPolicyRepository(db)
	migrationRepo := repositories.NewMigrationRepository(db)

	// Initialize storage backend for reports and backups
	storageBackend, err := storage.NewBackend(&cfg.Storage)
	if err != nil {
		log.Fatalf("Failed to initialize storage backend: %v", err)
	}

	// Phase 2 handlers
	reportHandlers := reports.NewHandlers(cfg, reportRepo, analysisRunRepo, analysisResultRepo, storageBackend)
	dashboardHandlers := dashboards.NewHandlers(db, analysisRunRepo, analysisResultRepo)

	// Phase 3 services and handlers
	snapshotService := snapshotSvc.NewService(snapshotRepo, driftRepo)

	// Phase 4 services and handlers
	backupService := backup.NewService(backupRepo, retentionPolicyRepo, sourceRepo, storageBackend)

	taskScheduler := schedulerSvc.New(scheduledTaskRepo, analysisRunRepo, analysisResultRepo, sourceRepo, snapshotService, backupService)
	taskScheduler.Start()

	schedulerHandlers := schedulerAPI.NewHandlers(scheduledTaskRepo, taskScheduler)
	snapshotHandlers := snapshots.NewHandlers(snapshotRepo, driftRepo, analysisResultRepo, analysisRunRepo, snapshotService)

	// (Phase 4 backup service already initialized above)
	// Build per-job storage backends from the migration job's source_config /
	// target_config so each side resolves to its own connection details.
	migrationService := migration.NewService(migrationRepo, storage.NewBackendFromRawConfig, nil)

	backupHandlers := backupsAPI.NewHandlers(cfg, db, backupService, retentionPolicyRepo, backupRepo, sourceRepo, tokenCipher)
	migrationHandlers := migrationsAPI.NewHandlers(cfg, migrationService, migrationRepo)

	// Phase 5 repositories
	notificationChannelRepo := repositories.NewNotificationChannelRepository(db)
	alertRuleRepo := repositories.NewAlertRuleRepository(db)
	alertsRepo := repositories.NewAlertsRepository(db)
	compliancePolicyRepo := repositories.NewCompliancePolicyRepository(db)
	complianceResultRepo := repositories.NewComplianceResultRepository(db)

	// Phase 5 services
	notificationService := notification.NewService(notificationChannelRepo)
	complianceChecker := complianceSvc.NewChecker(compliancePolicyRepo, complianceResultRepo)

	// Phase 5 handlers
	alertHandlers := alertsAPI.NewHandlers(alertsRepo, alertRuleRepo, notificationService)
	notificationHandlers := notificationsAPI.NewHandlers(notificationChannelRepo, notificationService)
	complianceHandlers := complianceAPI.NewHandlers(compliancePolicyRepo, complianceResultRepo, complianceChecker)
	webhookHandlers := webhooksAPI.NewHandlers(sourceRepo, analysisRunRepo)

	// Initialize rate limiters
	authRateLimiter := middleware.NewRateLimiter(middleware.AuthRateLimitConfig())
	generalRateLimiter := middleware.NewRateLimiter(middleware.DefaultRateLimitConfig())

	// API v1 routes
	apiV1 := router.Group("/api/v1")
	{
		// Setup status endpoint (public, no auth required)
		apiV1.GET("/setup/status", setupHandlers.GetSetupStatus)

		// Setup wizard endpoints (setup token auth, rate limited)
		// Permanently disabled after initial setup completes.
		setupGroup := apiV1.Group("/setup")
		setupGroup.Use(middleware.SetupTokenMiddleware(oidcConfigRepo))
		{
			setupGroup.POST("/validate-token", setupHandlers.ValidateToken)
			setupGroup.POST("/oidc/test", setupHandlers.TestOIDCConfig)
			setupGroup.POST("/oidc", setupHandlers.SaveOIDCConfig)
			setupGroup.POST("/admin", setupHandlers.ConfigureAdmin)
			setupGroup.POST("/complete", setupHandlers.CompleteSetup)
		}

		// Public authentication endpoints (no auth required, but rate limited)
		authGroup := apiV1.Group("/auth")
		authGroup.Use(middleware.RateLimitMiddleware(authRateLimiter))
		{
			authGroup.GET("/login", authHandlers.LoginHandler())
			authGroup.GET("/callback", authHandlers.CallbackHandler())
			authGroup.GET("/logout", authHandlers.LogoutHandler())
			authGroup.GET("/providers", authHandlers.ProvidersHandler())
		}

		// Development-only endpoints (gated by DEV_MODE; 403 in production).
		// Ported 1:1 from the registry so the frontend's dev-mode probes resolve.
		devGroup := apiV1.Group("/dev")
		devGroup.Use(admin.DevModeMiddleware())
		{
			devHandlers := admin.NewDevHandlers(cfg, identityDB)
			devGroup.GET("/status", devHandlers.DevStatusHandler())
			devGroup.POST("/login", devHandlers.DevLoginHandler())

			// Impersonation requires authentication + admin scope.
			devGroup.Use(middleware.AuthMiddleware(cfg, userRepo, apiKeyRepo, orgRepo))
			devGroup.GET("/users", middleware.RequireScope(auth.ScopeAdmin), devHandlers.ListUsersForImpersonationHandler())
			devGroup.POST("/impersonate/:user_id", middleware.RequireScope(auth.ScopeAdmin), devHandlers.ImpersonateUserHandler())
		}

		// Authenticated-only endpoints
		authenticatedGroup := apiV1.Group("")
		authenticatedGroup.Use(middleware.AuthMiddleware(cfg, userRepo, apiKeyRepo, orgRepo))
		authenticatedGroup.Use(middleware.RateLimitMiddleware(generalRateLimiter))
		authenticatedGroup.Use(middleware.AuditMiddleware(auditRepo))
		{
			// Auth endpoints requiring authentication
			authenticatedGroup.POST("/auth/refresh", authHandlers.RefreshHandler())
			authenticatedGroup.GET("/auth/me", authHandlers.MeHandler())

			// Dashboard stats
			authenticatedGroup.GET("/admin/stats/dashboard", statsHandlers.GetDashboardStats)

			// API Keys management (self-service for own keys)
			apiKeysGroup := authenticatedGroup.Group("/apikeys")
			{
				apiKeysGroup.GET("", apiKeyHandlers.ListAPIKeysHandler())
				apiKeysGroup.POST("", apiKeyHandlers.CreateAPIKeyHandler())
				apiKeysGroup.GET("/:id", apiKeyHandlers.GetAPIKeyHandler())
				apiKeysGroup.PUT("/:id", apiKeyHandlers.UpdateAPIKeyHandler())
				apiKeysGroup.DELETE("/:id", apiKeyHandlers.DeleteAPIKeyHandler())
				apiKeysGroup.POST("/:id/rotate", apiKeyHandlers.RotateAPIKeyHandler())
			}

			// Self-service user endpoints (any authenticated user)
			authenticatedGroup.GET("/users/me/memberships", userHandlers.GetCurrentUserMembershipsHandler())

			// Users management - read operations (requires users:read scope)
			usersGroup := authenticatedGroup.Group("/users")
			usersGroup.Use(middleware.RequireScope(auth.ScopeUsersRead))
			{
				usersGroup.GET("", userHandlers.ListUsersHandler())
				usersGroup.GET("/search", userHandlers.SearchUsersHandler())
				usersGroup.GET("/:id", userHandlers.GetUserHandler())
				usersGroup.GET("/:id/memberships", userHandlers.GetUserMembershipsHandler())
			}

			// Users management - write operations (requires users:write scope)
			usersWriteGroup := authenticatedGroup.Group("/users")
			usersWriteGroup.Use(middleware.RequireScope(auth.ScopeUsersWrite))
			{
				usersWriteGroup.POST("", userHandlers.CreateUserHandler())
				usersWriteGroup.PUT("/:id", userHandlers.UpdateUserHandler())
				usersWriteGroup.DELETE("/:id", userHandlers.DeleteUserHandler())
			}

			// GDPR data-subject endpoints (admin scope — reveal/destroy PII).
			adminUsersGroup := authenticatedGroup.Group("/admin/users")
			adminUsersGroup.Use(middleware.RequireScope(auth.ScopeAdmin))
			{
				adminUsersGroup.GET("/:id/export", gdprHandlers.ExportUserDataHandler())
				adminUsersGroup.POST("/:id/erase", gdprHandlers.EraseUserHandler())
			}

			// Organizations management
			orgsGroup := authenticatedGroup.Group("/organizations")
			{
				// Read operations require organizations:read
				orgsGroup.GET("", middleware.RequireScope(auth.ScopeOrganizationsRead), orgHandlers.ListOrganizationsHandler())
				orgsGroup.GET("/search", middleware.RequireScope(auth.ScopeOrganizationsRead), orgHandlers.SearchOrganizationsHandler())
				orgsGroup.GET("/:id", middleware.RequireScope(auth.ScopeOrganizationsRead), orgHandlers.GetOrganizationHandler())
				orgsGroup.GET("/:id/members", middleware.RequireScope(auth.ScopeOrganizationsRead), orgHandlers.ListMembersHandler())

				// Write operations require organizations:write
				orgsGroup.POST("", middleware.RequireScope(auth.ScopeOrganizationsWrite), orgHandlers.CreateOrganizationHandler())
				orgsGroup.PUT("/:id", middleware.RequireScope(auth.ScopeOrganizationsWrite), orgHandlers.UpdateOrganizationHandler())
				orgsGroup.DELETE("/:id", middleware.RequireScope(auth.ScopeOrganizationsWrite), orgHandlers.DeleteOrganizationHandler())

				// Member management requires organizations:write
				orgsGroup.POST("/:id/members", middleware.RequireScope(auth.ScopeOrganizationsWrite), orgHandlers.AddMemberHandler())
				orgsGroup.PUT("/:id/members/:user_id", middleware.RequireScope(auth.ScopeOrganizationsWrite), orgHandlers.UpdateMemberHandler())
				orgsGroup.DELETE("/:id/members/:user_id", middleware.RequireScope(auth.ScopeOrganizationsWrite), orgHandlers.RemoveMemberHandler())
			}

			// Role templates management
			roleTemplatesGroup := authenticatedGroup.Group("/admin/role-templates")
			{
				// Read operations - any authenticated user
				roleTemplatesGroup.GET("", roleTemplateHandlers.ListRoleTemplates)
				roleTemplatesGroup.GET("/:id", roleTemplateHandlers.GetRoleTemplate)

				// Write operations require admin scope
				roleTemplatesGroup.POST("", middleware.RequireScope(auth.ScopeAdmin), roleTemplateHandlers.CreateRoleTemplate)
				roleTemplatesGroup.PUT("/:id", middleware.RequireScope(auth.ScopeAdmin), roleTemplateHandlers.UpdateRoleTemplate)
				roleTemplatesGroup.DELETE("/:id", middleware.RequireScope(auth.ScopeAdmin), roleTemplateHandlers.DeleteRoleTemplate)
			}

			// OIDC settings management (admin-only)
			oidcAdminGroup := authenticatedGroup.Group("/admin/oidc")
			oidcAdminGroup.Use(middleware.RequireScope(auth.ScopeAdmin))
			{
				oidcAdminGroup.GET("/config", oidcAdminHandlers.GetActiveOIDCConfig)
				oidcAdminGroup.PUT("/group-mapping", oidcAdminHandlers.UpdateGroupMapping)
			}

			// Audit logs (read-only, admin)
			auditLogsGroup := authenticatedGroup.Group("/admin/audit-logs")
			auditLogsGroup.Use(middleware.RequireScope(auth.ScopeAuditRead))
			{
				auditLogsGroup.GET("", auditLogHandlers.ListAuditLogsHandler())
				auditLogsGroup.GET("/:id", auditLogHandlers.GetAuditLogHandler())
			}

			// ---------------------------------------------------------------
			// Phase 1: State Sources
			// ---------------------------------------------------------------
			sourcesGroup := authenticatedGroup.Group("/sources")
			{
				sourcesGroup.GET("", middleware.RequireScope(auth.ScopeSourcesRead), sourceHandlers.ListSources)
				sourcesGroup.POST("", middleware.RequireScope(auth.ScopeSourcesWrite), sourceHandlers.CreateSource)
				sourcesGroup.GET("/:id", middleware.RequireScope(auth.ScopeSourcesRead), sourceHandlers.GetSource)
				sourcesGroup.PUT("/:id", middleware.RequireScope(auth.ScopeSourcesWrite), sourceHandlers.UpdateSource)
				sourcesGroup.DELETE("/:id", middleware.RequireScope(auth.ScopeSourcesWrite), sourceHandlers.DeleteSource)
				sourcesGroup.POST("/:id/test", middleware.RequireScope(auth.ScopeSourcesWrite), sourceHandlers.TestSource)
			}

			// ---------------------------------------------------------------
			// Phase 1: Analysis
			// ---------------------------------------------------------------
			analysisGroup := authenticatedGroup.Group("/analysis")
			{
				analysisGroup.POST("/run", middleware.RequireScope(auth.ScopeAnalysisWrite), analysisHandlers.StartRun)
				analysisGroup.GET("/runs", middleware.RequireScope(auth.ScopeAnalysisRead), analysisHandlers.ListRuns)
				analysisGroup.GET("/runs/:id", middleware.RequireScope(auth.ScopeAnalysisRead), analysisHandlers.GetRun)
				analysisGroup.GET("/runs/:id/results", middleware.RequireScope(auth.ScopeAnalysisRead), analysisHandlers.GetRunResults)
				analysisGroup.POST("/runs/:id/cancel", middleware.RequireScope(auth.ScopeAnalysisWrite), analysisHandlers.CancelRun)
				analysisGroup.DELETE("/runs/:id", middleware.RequireScope(auth.ScopeAnalysisWrite), analysisHandlers.DeleteRun)
				analysisGroup.GET("/summary", middleware.RequireScope(auth.ScopeAnalysisRead), analysisHandlers.GetLatestSummary)
			}

			// ---------------------------------------------------------------
			// Phase 2: Reports
			// ---------------------------------------------------------------
			reportsGroup := authenticatedGroup.Group("/reports")
			{
				reportsGroup.POST("/generate", middleware.RequireScope(auth.ScopeReportsWrite), reportHandlers.GenerateReport)
				reportsGroup.GET("", middleware.RequireScope(auth.ScopeReportsRead), reportHandlers.ListReports)
				reportsGroup.GET("/:id", middleware.RequireScope(auth.ScopeReportsRead), reportHandlers.GetReport)
				reportsGroup.GET("/:id/download", middleware.RequireScope(auth.ScopeReportsRead), reportHandlers.DownloadReport)
				reportsGroup.DELETE("/:id", middleware.RequireScope(auth.ScopeReportsWrite), reportHandlers.DeleteReport)
			}

			// ---------------------------------------------------------------
			// Phase 2: Dashboard
			// ---------------------------------------------------------------
			dashboardGroup := authenticatedGroup.Group("/dashboard")
			dashboardGroup.Use(middleware.RequireScope(auth.ScopeDashboardRead))
			{
				dashboardGroup.GET("/overview", dashboardHandlers.GetOverview)
				dashboardGroup.GET("/resources", dashboardHandlers.GetResourceBreakdown)
				dashboardGroup.GET("/providers", dashboardHandlers.GetProviderDistribution)
				dashboardGroup.GET("/trends", dashboardHandlers.GetTrends)
				dashboardGroup.GET("/terraform-versions", dashboardHandlers.GetTerraformVersions)
				dashboardGroup.GET("/organizations", dashboardHandlers.GetOrganizationBreakdown)
				dashboardGroup.GET("/workspaces", dashboardHandlers.GetWorkspaceHealth)
			}

			// ---------------------------------------------------------------
			// Phase 3: Scheduler
			// ---------------------------------------------------------------
			schedulerGroup := authenticatedGroup.Group("/scheduler/tasks")
			schedulerGroup.Use(middleware.RequireScope(auth.ScopeSchedulerAdmin))
			{
				schedulerGroup.GET("", schedulerHandlers.ListTasks)
				schedulerGroup.POST("", schedulerHandlers.CreateTask)
				schedulerGroup.GET("/:id", schedulerHandlers.GetTask)
				schedulerGroup.PUT("/:id", schedulerHandlers.UpdateTask)
				schedulerGroup.DELETE("/:id", schedulerHandlers.DeleteTask)
				schedulerGroup.POST("/:id/trigger", schedulerHandlers.TriggerTask)
			}

			// ---------------------------------------------------------------
			// Phase 3: Snapshots and Drift
			// ---------------------------------------------------------------
			snapshotsGroup := authenticatedGroup.Group("/snapshots")
			{
				snapshotsGroup.GET("", middleware.RequireScope(auth.ScopeAnalysisRead), snapshotHandlers.ListSnapshots)
				snapshotsGroup.GET("/:id", middleware.RequireScope(auth.ScopeAnalysisRead), snapshotHandlers.GetSnapshot)
				snapshotsGroup.POST("/capture", middleware.RequireScope(auth.ScopeAnalysisWrite), snapshotHandlers.CaptureNow)
				snapshotsGroup.GET("/compare", middleware.RequireScope(auth.ScopeAnalysisRead), snapshotHandlers.CompareSnapshots)
			}

			driftGroup := authenticatedGroup.Group("/drift")
			driftGroup.Use(middleware.RequireScope(auth.ScopeAnalysisRead))
			{
				driftGroup.GET("/events", snapshotHandlers.ListDriftEvents)
				driftGroup.GET("/events/:id", snapshotHandlers.GetDriftEvent)
			}

			// ---------------------------------------------------------------
			// Phase 4: Backups
			// ---------------------------------------------------------------
			backupsGroup := authenticatedGroup.Group("/backups")
			{
				backupsGroup.GET("", middleware.RequireScope(auth.ScopeBackupsRead), backupHandlers.ListBackups)
				backupsGroup.POST("/create", middleware.RequireScope(auth.ScopeBackupsWrite), backupHandlers.CreateBackup)
				backupsGroup.POST("/create-bulk", middleware.RequireScope(auth.ScopeBackupsWrite), backupHandlers.CreateBulkBackup)
				backupsGroup.GET("/:id", middleware.RequireScope(auth.ScopeBackupsRead), backupHandlers.GetBackup)
				backupsGroup.DELETE("/:id", middleware.RequireScope(auth.ScopeBackupsWrite), backupHandlers.DeleteBackup)
				backupsGroup.POST("/:id/restore", middleware.RequireScope(auth.ScopeBackupsWrite), backupHandlers.RestoreBackup)
				backupsGroup.POST("/:id/verify", middleware.RequireScope(auth.ScopeBackupsRead), backupHandlers.VerifyBackup)

				// Retention policy sub-group
				retentionGroup := backupsGroup.Group("/retention")
				retentionGroup.Use(middleware.RequireScope(auth.ScopeBackupsWrite))
				{
					retentionGroup.GET("", backupHandlers.ListRetentionPolicies)
					retentionGroup.POST("", backupHandlers.CreateRetentionPolicy)
					retentionGroup.GET("/:id", backupHandlers.GetRetentionPolicy)
					retentionGroup.PUT("/:id", backupHandlers.UpdateRetentionPolicy)
					retentionGroup.DELETE("/:id", backupHandlers.DeleteRetentionPolicy)
					retentionGroup.POST("/apply", backupHandlers.ApplyRetention)
				}
			}

			// ---------------------------------------------------------------
			// Phase 4: Migrations
			// ---------------------------------------------------------------
			migrationsGroup := authenticatedGroup.Group("/migrations")
			{
				migrationsGroup.POST("", middleware.RequireScope(auth.ScopeMigrationsWrite), migrationHandlers.CreateMigration)
				migrationsGroup.GET("", middleware.RequireScope(auth.ScopeMigrationsRead), migrationHandlers.ListMigrations)
				migrationsGroup.GET("/:id", middleware.RequireScope(auth.ScopeMigrationsRead), migrationHandlers.GetMigration)
				migrationsGroup.POST("/:id/cancel", middleware.RequireScope(auth.ScopeMigrationsWrite), migrationHandlers.CancelMigration)
				migrationsGroup.POST("/validate", middleware.RequireScope(auth.ScopeMigrationsWrite), migrationHandlers.ValidateMigration)
				migrationsGroup.POST("/dry-run", middleware.RequireScope(auth.ScopeMigrationsWrite), migrationHandlers.DryRunMigration)
			}

			// ---------------------------------------------------------------
			// Phase 5: Alerts
			// ---------------------------------------------------------------
			alertsGroup := authenticatedGroup.Group("/alerts")
			alertsGroup.Use(middleware.RequireScope(auth.ScopeAlertsAdmin))
			{
				alertsGroup.GET("", alertHandlers.ListAlerts)
				alertsGroup.PUT("/:id/acknowledge", alertHandlers.AcknowledgeAlert)

				// Alert rules sub-group
				alertRulesGroup := alertsGroup.Group("/rules")
				{
					alertRulesGroup.GET("", alertHandlers.ListAlertRules)
					alertRulesGroup.POST("", alertHandlers.CreateAlertRule)
					alertRulesGroup.GET("/:id", alertHandlers.GetAlertRule)
					alertRulesGroup.PUT("/:id", alertHandlers.UpdateAlertRule)
					alertRulesGroup.DELETE("/:id", alertHandlers.DeleteAlertRule)
				}
			}

			// ---------------------------------------------------------------
			// Phase 5: Notifications
			// ---------------------------------------------------------------
			notificationsGroup := authenticatedGroup.Group("/notifications/channels")
			notificationsGroup.Use(middleware.RequireScope(auth.ScopeAlertsAdmin))
			{
				notificationsGroup.GET("", notificationHandlers.ListChannels)
				notificationsGroup.POST("", notificationHandlers.CreateChannel)
				notificationsGroup.GET("/:id", notificationHandlers.GetChannel)
				notificationsGroup.PUT("/:id", notificationHandlers.UpdateChannel)
				notificationsGroup.DELETE("/:id", notificationHandlers.DeleteChannel)
				notificationsGroup.POST("/:id/test", notificationHandlers.TestChannel)
			}

			// ---------------------------------------------------------------
			// Phase 5: Compliance
			// ---------------------------------------------------------------
			compliancePoliciesGroup := authenticatedGroup.Group("/compliance/policies")
			{
				compliancePoliciesGroup.GET("", middleware.RequireScope(auth.ScopeComplianceRead), complianceHandlers.ListPolicies)
				compliancePoliciesGroup.POST("", middleware.RequireScope(auth.ScopeComplianceWrite), complianceHandlers.CreatePolicy)
				compliancePoliciesGroup.GET("/:id", middleware.RequireScope(auth.ScopeComplianceRead), complianceHandlers.GetPolicy)
				compliancePoliciesGroup.PUT("/:id", middleware.RequireScope(auth.ScopeComplianceWrite), complianceHandlers.UpdatePolicy)
				compliancePoliciesGroup.DELETE("/:id", middleware.RequireScope(auth.ScopeComplianceWrite), complianceHandlers.DeletePolicy)
			}
			authenticatedGroup.GET("/compliance/results", middleware.RequireScope(auth.ScopeComplianceRead), complianceHandlers.ListResults)
			authenticatedGroup.GET("/compliance/score", middleware.RequireScope(auth.ScopeComplianceRead), complianceHandlers.GetComplianceScore)

			// ---------------------------------------------------------------
			// Phase 5: Webhooks (CI/CD triggers)
			// ---------------------------------------------------------------
			authenticatedGroup.POST("/webhooks/trigger", middleware.RequireScope(auth.ScopeAnalysisWrite), webhookHandlers.TriggerAnalysis)
		}
	}

	bg := &BackgroundServices{
		rateLimiters: []*middleware.RateLimiter{authRateLimiter, generalRateLimiter},
		scheduler:    taskScheduler,
	}

	return router, bg
}

// LoggerMiddleware provides structured logging for every HTTP request.
func LoggerMiddleware(cfg *config.Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		path := c.Request.URL.Path
		query := c.Request.URL.RawQuery

		c.Next()

		latency := time.Since(start)

		if cfg.Logging.Format == "json" {
			logJSON(c, latency, path, query)
		} else {
			logText(c, latency, path, query)
		}
	}
}

// logJSON logs a request as a JSON-structured slog record.
func logJSON(c *gin.Context, latency time.Duration, path, query string) {
	requestID, _ := c.Get(middleware.RequestIDKey)
	slog.LogAttrs(
		c.Request.Context(),
		slog.LevelInfo,
		"http request",
		slog.String("method", c.Request.Method),
		slog.String("path", path),
		slog.String("query", query),
		slog.Int("status", c.Writer.Status()),
		slog.Int("size", c.Writer.Size()),
		slog.Duration("latency", latency),
		slog.String("ip", c.ClientIP()),
		slog.String("request_id", fmt.Sprintf("%v", requestID)),
		slog.String("user_agent", c.Request.UserAgent()),
	)
}

// logText logs a request as a human-readable slog text record.
func logText(c *gin.Context, latency time.Duration, path, query string) {
	// Reuse the same structured output; slog will emit text format when the global
	// handler is a TextHandler (configured in telemetry.SetupLogger).
	logJSON(c, latency, path, query)
}

// CORSMiddleware handles Cross-Origin Resource Sharing headers.
func CORSMiddleware(cfg *config.Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		origin := c.Request.Header.Get("Origin")

		// Check if origin is allowed
		allowed := false
		for _, allowedOrigin := range cfg.Security.CORS.AllowedOrigins {
			if allowedOrigin == "*" || allowedOrigin == origin {
				allowed = true
				break
			}
		}

		if allowed {
			if origin == "" {
				c.Header("Access-Control-Allow-Origin", "*")
			} else {
				c.Header("Access-Control-Allow-Origin", origin)
			}
			c.Header("Access-Control-Allow-Credentials", "true")
			c.Header("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
			c.Header("Access-Control-Allow-Headers", "Origin, Content-Type, Accept, Authorization, X-Requested-With")
			c.Header("Access-Control-Max-Age", "3600")
		}

		if c.Request.Method == "OPTIONS" {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}

		c.Next()
	}
}
