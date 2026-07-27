// @title           Terraform State Manager API
// @version         1.0.0
// @description     REST API for the Terraform State Manager: analyze, edit, and move Terraform state across existing backends, with CI-driven drift detection and version testing.
// @basePath        /api/v1
// @schemes         https http
//
// @securityDefinitions.apikey  BearerAuth
// @in                          header
// @name                        Authorization
// @description                 Session JWT. Format: "Bearer {token}". The same token is also delivered as the HttpOnly tsm_auth_token cookie.
//
// @securityDefinitions.apikey  CookieAuth
// @in                          cookie
// @name                        tsm_auth_token
// @description                 HttpOnly session cookie set at login. Mutating requests additionally require the X-CSRF-Token header to match the tsm_csrf cookie (double-submit).
//
// @securityDefinitions.apikey  CallbackToken
// @in                          header
// @name                        X-TSM-Callback-Token
// @description                 Per-run one-shot token authenticating a CI job's machine callback (drift/health results).

// Package main is the entry point for the Terraform State Manager server binary.
// It dispatches three subcommands — serve, migrate, and version — via a simple
// switch on os.Args so the binary's full CLI surface is readable in one place
// without requiring a cobra dependency. The serve command runs auto-migration on
// startup so freshly deployed containers never need a separate migration step.
//
// This mirrors the structure of the sibling terraform-registry-backend so the two
// services share operational conventions (config layering, embedded migrations,
// side-channel metrics port, graceful shutdown).
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/sethbacon/terraform-suite-identity/identity"
	idstore "github.com/sethbacon/terraform-suite-identity/identity/store"
	"golang.org/x/crypto/bcrypt"

	"github.com/terraform-state-manager/terraform-state-manager/internal/api"
	"github.com/terraform-state-manager/terraform-state-manager/internal/api/setup"
	"github.com/terraform-state-manager/terraform-state-manager/internal/auth"
	"github.com/terraform-state-manager/terraform-state-manager/internal/bootstrap"
	"github.com/terraform-state-manager/terraform-state-manager/internal/config"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
	"github.com/terraform-state-manager/terraform-state-manager/internal/statesource"
	"github.com/terraform-state-manager/terraform-state-manager/internal/telemetry"
)

// Version and BuildDate are injected at build time via ldflags:
//
//	-X main.Version=x.y.z  -X main.BuildDate=<RFC3339>
var (
	Version   = "dev"
	BuildDate = "unknown"
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("Error: %v\n", err)
	}
}

func run() error {
	command := "serve"
	if len(os.Args) > 1 {
		command = os.Args[1]
	}

	cfg, err := config.Load(os.Getenv("CONFIG_PATH"))
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	switch command {
	case "serve":
		api.AppVersion = Version
		api.AppBuildDate = BuildDate
		return serve(cfg)
	case "migrate":
		if len(os.Args) < 3 {
			return fmt.Errorf("usage: %s migrate <up|down>", os.Args[0])
		}
		return runMigrations(cfg, os.Args[2])
	case "version":
		fmt.Printf("Terraform State Manager v%s (built %s)\n", Version, BuildDate)
		return nil
	default:
		return fmt.Errorf("unknown command: %s\nAvailable commands: serve, migrate, version", command)
	}
}

func serve(cfg *config.Config) error {
	// Fail fast on a misconfiguration (before touching logging or the DB) so an
	// operator sees the problem at boot rather than as a first-use crash.
	if err := cfg.Validate(); err != nil {
		return err
	}

	// Initialise structured logging as early as possible so all later output uses
	// the configured format/level.
	telemetry.SetupLogger(cfg.Logging.Format, cfg.Logging.Level)

	// Apply the operator's SSRF egress allow-list to the state-source connectors
	// (empty leaves the built-in private-range default) and pin the git-clone
	// transport to the same dial-time guard. Must run before any connector is
	// constructed (the guard is a package global).
	if len(cfg.Security.Egress.Allowlist) > 0 {
		if err := statesource.ConfigureEgress(cfg.Security.Egress.Allowlist); err != nil {
			return fmt.Errorf("invalid security.egress.allowlist: %w", err)
		}
		slog.Info("state-source egress allow-list applied", "entries", len(cfg.Security.Egress.Allowlist))
	}
	statesource.InstallGuardedGitTransport()

	// Export build information as a Prometheus metric for fleet inventory queries.
	telemetry.AppInfo.WithLabelValues(Version, runtime.Version(), BuildDate).Set(1)

	if cfg.Logging.Level == "debug" {
		gin.SetMode(gin.DebugMode)
	} else {
		gin.SetMode(gin.ReleaseMode)
	}

	// Resolve and validate the JWT signing secret (fails fast in production).
	if err := auth.ValidateJWTSecret(); err != nil {
		return fmt.Errorf("auth configuration error: %w", err)
	}

	database, err := db.Connect(cfg.Database.GetDSN(), cfg.Database.MaxConnections, cfg.Database.MinIdleConnections)
	if err != nil {
		return fmt.Errorf("failed to connect to database: %w", err)
	}
	defer func() { _ = database.Close() }()
	slog.Info("connected to database")

	stopDBStats := telemetry.StartDBStatsCollector(database)
	defer stopDBStats()

	slog.Info("running database migrations")
	if err := db.RunMigrations(database, "up"); err != nil {
		return fmt.Errorf("failed to run migrations: %w", err)
	}
	if version, dirty, verr := db.GetMigrationVersion(database); verr == nil {
		slog.Info("database schema ready", "version", version, "dirty", dirty)
	}

	// Seed the built-in CI workflow templates so operators have an editable
	// baseline in the store (idempotent; falls back to embedded consts if absent).
	if err := api.SeedWorkflowTemplates(context.Background(), database); err != nil {
		return fmt.Errorf("failed to seed workflow templates: %w", err)
	}

	// Generate the first-run setup-wizard token if setup isn't complete. The raw
	// token is printed to stdout (and optional SETUP_TOKEN_FILE); only its bcrypt
	// hash is stored, and it's cleared when the wizard completes.
	if err := handleSetupToken(repositories.NewSystemSettingsRepository(database)); err != nil {
		return fmt.Errorf("failed to prepare setup token: %w", err)
	}

	// Shared identity schema (terraform-suite-identity). The identity store lives
	// in a separate/shared database when TSM_IDENTITY_DATABASE_* is set, otherwise
	// the app database (default). Open the identity connection first, then run its
	// migrations and bootstrap against it. The migrations are schema-qualified
	// (identity.*), so running them on this search_path pool matches the previous
	// app-pool behavior when the identity and app databases coincide (the default).
	identityDB, err := db.Connect(
		cfg.IdentityDatabase.GetDSNWithSearchPath("identity,public"),
		cfg.IdentityDatabase.MaxConnections, cfg.IdentityDatabase.MinIdleConnections,
	)
	if err != nil {
		return fmt.Errorf("failed to connect to identity schema: %w", err)
	}
	defer func() { _ = identityDB.Close() }()

	slog.Info("running identity schema migrations")
	if err := identity.RunMigrations(identityDB, "up"); err != nil {
		return fmt.Errorf("failed to run identity migrations: %w", err)
	}
	// Under a shared identity database, only the designated app seeds role
	// templates (suite.role_seed_owner) to avoid clobbering the sibling's role
	// scopes. Default "self" seeds as today. The default org is always ensured.
	if err := bootstrap.Run(context.Background(), identityDB, cfg.Suite.ShouldSeedRoles("tsm")); err != nil {
		return fmt.Errorf("failed to bootstrap identity data: %w", err)
	}
	slog.Info("identity schema ready (role templates + default org seeded)")

	// Daily cleanup of expired JWT revocation entries.
	tokenRepo := idstore.NewTokenRepository(identityDB)
	go func() {
		ticker := time.NewTicker(24 * time.Hour)
		defer ticker.Stop()
		for range ticker.C {
			if err := tokenRepo.CleanupExpiredRevocations(context.Background()); err != nil {
				slog.Error("failed to clean up expired token revocations", "error", err)
			}
		}
	}()

	// Prometheus metrics on a dedicated side-channel port so the scrape path is
	// off the public API ingress and never rate-limited.
	if cfg.Telemetry.Metrics.Enabled {
		metricsAddr := fmt.Sprintf(":%d", cfg.Telemetry.Metrics.PrometheusPort)
		go func() {
			mux := http.NewServeMux()
			mux.Handle("/metrics", promhttp.Handler())
			slog.Info("starting metrics server", "addr", metricsAddr)
			srv := &http.Server{
				Addr:         metricsAddr,
				Handler:      mux,
				ReadTimeout:  10 * time.Second,
				WriteTimeout: 10 * time.Second,
			}
			if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				slog.Error("metrics server error", "error", err)
			}
		}()
	}

	router, stopWorkers, err := api.NewRouter(cfg, database, identityDB)
	if err != nil {
		return fmt.Errorf("failed to build router: %w", err)
	}
	defer stopWorkers() // halt the background schedule runner on shutdown

	server := &http.Server{
		Addr:              cfg.Server.GetAddress(),
		Handler:           router,
		ReadTimeout:       cfg.Server.ReadTimeout,
		WriteTimeout:      cfg.Server.WriteTimeout,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	// Serve HTTPS when a cert/key is configured. When mTLS is also enabled, load
	// the client CA pool and verify presented client certs against it (optional,
	// so other auth methods still work). Go populates ConnectionState.VerifiedChains
	// only for certs that pass this verification — which the mTLS middleware requires.
	useTLS := cfg.Server.TLSCertFile != "" && cfg.Server.TLSKeyFile != ""
	if useTLS && cfg.Auth.MTLS.Enabled {
		caPool, err := loadClientCAPool(cfg.Auth.MTLS.ClientCAFile)
		if err != nil {
			return fmt.Errorf("failed to load mTLS client CA: %w", err)
		}
		server.TLSConfig = &tls.Config{
			ClientCAs:  caPool,
			ClientAuth: tls.VerifyClientCertIfGiven,
			MinVersion: tls.VersionTLS12,
		}
	}

	go func() {
		slog.Info("server ready", "addr", cfg.Server.GetAddress(), "base_url", cfg.Server.BaseURL, "tls", useTLS, "mtls", cfg.Auth.MTLS.Enabled)
		var srvErr error
		if useTLS {
			srvErr = server.ListenAndServeTLS(cfg.Server.TLSCertFile, cfg.Server.TLSKeyFile)
		} else {
			srvErr = server.ListenAndServe()
		}
		if srvErr != nil && srvErr != http.ErrServerClosed {
			log.Fatalf("failed to start server: %v", srvErr)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	slog.Info("shutting down server")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		return fmt.Errorf("server forced to shutdown: %w", err)
	}
	slog.Info("server stopped gracefully")
	return nil
}

// loadClientCAPool reads a PEM bundle of trusted client-certificate CAs for mTLS.
func loadClientCAPool(caFile string) (*x509.CertPool, error) {
	pemBytes, err := os.ReadFile(caFile) // #nosec G304 -- operator-configured CA path
	if err != nil {
		return nil, fmt.Errorf("read client CA file %q: %w", caFile, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pemBytes) {
		return nil, fmt.Errorf("no certificates found in client CA file %q", caFile)
	}
	return pool, nil
}

func runMigrations(cfg *config.Config, direction string) error {
	database, err := db.Connect(cfg.Database.GetDSN(), cfg.Database.MaxConnections, cfg.Database.MinIdleConnections)
	if err != nil {
		return fmt.Errorf("failed to connect to database: %w", err)
	}
	defer func() { _ = database.Close() }()

	slog.Info("running migrations", "direction", direction)
	if err := db.RunMigrations(database, direction); err != nil {
		return fmt.Errorf("migration failed: %w", err)
	}
	version, dirty, err := db.GetMigrationVersion(database)
	if err != nil {
		return fmt.Errorf("failed to get migration version: %w", err)
	}
	slog.Info("migration complete", "version", version, "dirty", dirty)
	return nil
}

// handleSetupToken prepares the first-run setup-wizard token when setup is not
// yet complete. If SETUP_TOKEN is set, that operator-supplied token is used (and
// never echoed); otherwise a random token (prefix "tsm_setup_") is generated,
// printed to stdout, and — when SETUP_TOKEN_FILE is set — written to that file
// for container secret mounting. Only the bcrypt hash is persisted in
// system_settings. Idempotent: a pre-existing hash (restarted mid-setup) is left
// intact.
func handleSetupToken(repo *repositories.SystemSettingsRepository) error {
	ctx := context.Background()

	completed, err := repo.IsSetupCompleted(ctx)
	if err != nil {
		return fmt.Errorf("failed to check setup status: %w", err)
	}
	if completed {
		hasPending, perr := repo.HasPendingFeatureSetup(ctx)
		if perr != nil {
			return fmt.Errorf("failed to check pending feature setup: %w", perr)
		}
		if !hasPending {
			return nil // setup fully done
		}
	}

	if existing, gerr := repo.GetSetupTokenHash(ctx); gerr != nil {
		return fmt.Errorf("failed to check existing setup token: %w", gerr)
	} else if existing != "" {
		slog.Info("setup required: a setup token was already generated; delete setup_token_hash from system_settings and restart to regenerate")
		return nil
	}

	rawToken, generated, err := setup.ResolveSetupToken(os.Getenv("SETUP_TOKEN"))
	if err != nil {
		return err
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(rawToken), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("failed to hash setup token: %w", err)
	}
	if err := repo.SetSetupTokenHash(ctx, string(hash)); err != nil {
		return fmt.Errorf("failed to store setup token hash: %w", err)
	}

	// An operator-supplied token is already known to them: don't echo it or mirror
	// it to a file — just record that setup is required.
	if !generated {
		slog.Info("setup required: using the operator-supplied setup token (SETUP_TOKEN); complete setup at /setup")
		return nil
	}

	sep := strings.Repeat("=", 66)
	fmt.Println("\n" + sep)
	fmt.Println("  INITIAL SETUP REQUIRED")
	fmt.Printf("  Setup Token: %s\n", rawToken)
	fmt.Println("  Complete setup in the browser at /setup, or:")
	fmt.Println("    POST /api/v1/setup/validate-token   (Authorization: SetupToken <token>)")
	fmt.Println("  Single-use; invalidated after setup. Treat it like a root password.")
	fmt.Println(sep + "\n")

	// Optionally mirror the token to a file (container secret mount). The path is
	// operator-supplied config: reject traversal sequences, then clean it.
	if tokenFile := os.Getenv("SETUP_TOKEN_FILE"); tokenFile != "" {
		if strings.Contains(filepath.ToSlash(tokenFile), "..") {
			slog.Warn("SETUP_TOKEN_FILE contains path-traversal sequences; ignoring", "path", tokenFile)
		} else {
			clean := filepath.Clean(tokenFile)
			if werr := os.WriteFile(clean, []byte(rawToken), 0o600); werr != nil { // #nosec G306 G703 -- 0600 perms; SETUP_TOKEN_FILE is operator config, filepath.Clean'd and ".." rejected above
				slog.Warn("failed to write setup token file", "path", clean, "error", werr)
			} else {
				slog.Info("setup token written to file", "path", clean)
			}
		}
	}
	return nil
}
