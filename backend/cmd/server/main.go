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
	"database/sql"
	"errors"
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
	"github.com/terraform-state-manager/terraform-state-manager/internal/approles"
	"github.com/terraform-state-manager/terraform-state-manager/internal/auth"
	"github.com/terraform-state-manager/terraform-state-manager/internal/bootstrap"
	"github.com/terraform-state-manager/terraform-state-manager/internal/config"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
	"github.com/terraform-state-manager/terraform-state-manager/internal/egress"
	"github.com/terraform-state-manager/terraform-state-manager/internal/maintenance"
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
	case "bind-targets":
		// bind-targets [verify] — convert notification-channel targets to the
		// row-bound ciphertext form, or report how many are still unbound.
		verify := len(os.Args) > 2 && os.Args[2] == "verify"
		return runBindTargets(cfg, verify)
	case "rekey-targets":
		// rekey-targets [verify] — re-encrypt notification-channel targets under
		// the current TSM_ENCRYPTION_KEY, or report what still needs the
		// previous one.
		//
		// An unrecognised argument is rejected rather than ignored: this
		// command's default mode REWRITES stored credentials, and silently
		// reading `verfiy` as "no argument" would run the writing mode on a typo
		// of the read-only one.
		verify := false
		if len(os.Args) > 2 {
			if os.Args[2] != "verify" {
				return fmt.Errorf("usage: %s rekey-targets [verify]", os.Args[0])
			}
			verify = true
		}
		return runRekeyTargets(cfg, verify)
	case "reown-roots":
		// reown-roots verify | reown-roots move <from-org-uuid> <to-org-uuid>
		//
		// Re-owns the partition roots of #436 for rows written before the
		// acting-organization stamp existed. The mapping is an ARGUMENT and
		// never a default: this rewrites who owns a tenant's state sources,
		// credentials and schedules, and the one thing that must not happen is
		// moving rows nobody meant to move.
		//
		// Same typo discipline as rekey-targets, for the same reason and with
		// more at stake: an unrecognised sub-argument is an error, not a
		// silently-ignored one.
		if len(os.Args) < 3 {
			return fmt.Errorf("usage: %s reown-roots verify | %s reown-roots move <from-org-id> <to-org-id>",
				os.Args[0], os.Args[0])
		}
		switch os.Args[2] {
		case "verify":
			return runReownRoots(cfg, "", "")
		case "move":
			if len(os.Args) != 5 {
				return fmt.Errorf("usage: %s reown-roots move <from-org-id> <to-org-id>", os.Args[0])
			}
			return runReownRoots(cfg, os.Args[3], os.Args[4])
		default:
			return fmt.Errorf("usage: %s reown-roots verify | %s reown-roots move <from-org-id> <to-org-id>",
				os.Args[0], os.Args[0])
		}
	case "authz-drift":
		// authz-drift — compare this application's role tables against the shared
		// identity schema and exit non-zero while they disagree.
		//
		// READ-ONLY, and therefore takes no verify/convert argument: unlike
		// bind-targets and rekey-targets there is no writing mode to typo one's
		// way into. Repairing is approles.Reconcile's job, at boot.
		return runAuthzDrift(cfg)
	case "version":
		fmt.Printf("Terraform State Manager v%s (built %s)\n", Version, BuildDate)
		return nil
	default:
		return fmt.Errorf("unknown command: %s\nAvailable commands: serve, migrate, bind-targets, rekey-targets, reown-roots, authz-drift, version", command)
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

	// The SAME allow-list also governs the outbound requests the shared identity
	// module makes on this app's behalf — OIDC discovery, JWKS and the
	// authorization-code token exchange, the sibling-manifest poll, and the
	// module-freshness join — which identity v0.25.0 routes through httpsafe.
	// Unconditional, and applied even when the list is empty: empty means the
	// STRICT policy there (loopback, RFC 1918, link-local, CGNAT and IPv6 ULA
	// all denied), not the connectors' private-range default. A deployment whose
	// IdP or sibling app lives on an internal address must name it — and,
	// because setting the list REPLACES the connectors' default, must re-state
	// the private ranges those connectors still need. See internal/egress.
	if err := egress.Configure(cfg.Security.Egress.Allowlist); err != nil {
		return fmt.Errorf("invalid security.egress.allowlist: %w", err)
	}

	// Confine the state sources that name a path on this server's filesystem (the
	// local connector's base_path, the kubernetes connector's kubeconfig) to the
	// operator's permitted roots. Empty configuration fails closed — such sources
	// simply cannot be created. Like the egress guard these are package globals,
	// so this must run before any connector is constructed.
	if err := statesource.ConfigureLocalRoots(cfg.StateSource.LocalRoots); err != nil {
		return fmt.Errorf("invalid statesource.local_roots: %w", err)
	}
	if err := statesource.ConfigureKubeconfigRoots(cfg.StateSource.KubeconfigRoots); err != nil {
		return fmt.Errorf("invalid statesource.kubeconfig_roots: %w", err)
	}
	slog.Info("state-source filesystem roots applied",
		"local_roots", len(cfg.StateSource.LocalRoots),
		"kubeconfig_roots", len(cfg.StateSource.KubeconfigRoots))

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
	//
	// bootstrap.Run also reconciles TSM's OWN per-app authorization tables from
	// the result (internal/approles): it takes the app connection because those
	// tables live there, and it runs after the seed because what it copies is
	// what the seed just produced.
	//
	// The token-revocation repository is constructed here, ahead of the router's
	// own, because the reconcile needs it: a build that NARROWS a role template
	// ends the sessions of everyone holding it before the narrowing lands
	// (#557), and that write is the application's to make — approles declares
	// the contract and does not know what a credential is.
	if err := bootstrap.Run(context.Background(), identityDB, database, cfg.Suite.ShouldSeedRoles("tsm"),
		repositories.NewUserTokenRevocationRepository(database)); err != nil {
		return fmt.Errorf("failed to bootstrap identity data: %w", err)
	}
	slog.Info("identity schema ready (role templates + default org seeded)")

	// RECONCILE this application's own group_mappings table from the
	// sso_settings overlay (terraform-suite-identity#206 phase 2, migration
	// 000036) -- AFTER bootstrap.Run, because each mirrored row's
	// role_template_id is resolved against role_templates, which bootstrap's
	// reconcile and seed have just brought current. Same standing-reconcile
	// reasoning as that reconcile: 000036 ships no SQL backfill (a migration on
	// the app connection cannot see which connection resolves the live overlay
	// row), a re-derivation is a no-op when nothing changed, and it repairs
	// whatever a transient dual-write failure left behind. The identity pool is
	// the source side because it is the connection the auth handlers hand
	// NewSSOSettingsRepository. NOTHING READS THE TABLE YET, so a failure here
	// is logged, not fatal: requests are unaffected, only the backfill for the
	// eventual read cutover is stale.
	if report, gmErr := repositories.ReconcileGroupMappings(context.Background(), identityDB, database); gmErr != nil {
		slog.Error("could not reconcile this application's group_mappings from the sso_settings overlay; "+
			"nothing reads the table yet, so requests are unaffected, but the phase-2 backfill is stale "+
			"and mapping changes made while the live dual-write was failing are NOT repaired. Run `authz-drift`",
			"error", gmErr)
	} else {
		slog.Info("group mappings reconciled", "report", report)
	}

	// WHICH TABLES DECIDE AUTHORIZATION, IN THE STARTUP LOG.
	//
	// This is the same reasoning approles.Store.Verify applies to the resolved
	// table names: a deployment reading the wrong source is indistinguishable
	// from a correct one in every other observable, so the answer is stated once
	// at boot rather than inferred later from who can do what. Refused rather
	// than defaulted — an operator who typed `TSM_AUTHZ_ROLE_SOURCE=idenity` must
	// see it here, not discover months later that the flip never happened.
	roleSource, err := approles.ParseRoleSource(cfg.Authz.RoleSource)
	if err != nil {
		return err
	}
	slog.Info("authorization role source", "source", string(roleSource),
		"rollback", "set TSM_AUTHZ_ROLE_SOURCE=identity and restart to read the shared identity schema again")

	// The standing detector. It reports and never corrects; the repair is
	// bootstrap.Run's reconcile above, which has just run.
	stopDriftWatch := startAuthzDriftWatch(database, identityDB, cfg.Authz.DriftInterval)
	defer stopDriftWatch()

	// Daily cleanup of expired JWT revocation entries.
	tokenRepo := idstore.NewTokenRepository(identityDB)
	go func() {
		ticker := time.NewTicker(24 * time.Hour)
		defer ticker.Stop()
		for range ticker.C {
			// The sweep reports how many denylist rows it removed: zero is a
			// normal outcome (nothing had expired), not a miss, so it is a count
			// rather than an error.
			removed, err := tokenRepo.CleanupExpiredRevocations(context.Background())
			if err != nil {
				slog.Error("failed to clean up expired token revocations", "error", err)
				continue
			}
			slog.Debug("expired token revocations cleaned up", "rows", removed)
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

// runBindTargets converts notification-channel targets to the row-bound
// ciphertext form (suite-identity #153), or with "verify" reports how many
// remain unbound without writing anything.
//
// Operator-invoked rather than a startup hook: a boot-time sweep runs on every
// replica at once and would need a cross-replica claim to be safe, and this
// table is far too small for that to be worth it.
//
// The verify form is the exit criterion for the migration. While any row is
// unbound the notifier must keep accepting unbound ciphertexts, which is the
// property being retired; once verify reports zero, the read can move to
// OpenWithContext and the legacy acceptance can be deleted. It exits non-zero
// when rows remain, so it can gate that change in a script rather than needing
// someone to read the output.
// authzDriftWorker is the name the drift loop reports staleness under.
const authzDriftWorker = "authz-drift"

// startAuthzDriftWatch runs the role-drift comparison on an interval and reports
// what it finds. It returns a function that stops the loop.
//
// # Why a periodic reporter, and not a shadow read in the request path
//
// A shadow comparison inside the read path — resolve the scopes both ways on
// every derivation, compare, emit a metric — was the other candidate, and it is
// worse here for two reasons. It runs on the API-key hot path
// (internal/middleware re-derives a key owner's live scopes on EVERY request), so
// it would double that query per request to detect a condition that changes on
// the timescale of a membership write. And it only ever sees the principals who
// happen to authenticate: a service account that has silently kept an
// administrator role it should have lost is invisible to it until somebody uses
// the credential, which is the moment it stops mattering that it was detectable.
//
// This sees the whole estate, on a bounded schedule, whether anybody logs in or
// not. What it gives up is latency — a divergence introduced now is reported
// within one interval, not on the next request.
//
// # It reports; it does not correct
//
// Correcting here would make a background loop the thing that rewrites
// authorization, with no boundary an operator could put around it.
// approles.Reconcile corrects, at boot, and now says what it changed.
//
// # What it will and will not catch
//
// CATCHES: a mirror write whose identity leg committed and whose app leg did not;
// a membership removed by ON DELETE CASCADE when an organization or user is
// deleted; a row written to identity by the sibling registry; a role definition
// whose scopes differ between the two schemas.
//
// DOES NOT CATCH: a fault that corrupts BOTH sides identically (they agree, so
// there is nothing to compare); an authorization decision that is wrong for a
// reason other than the role — a stale JWT still carrying scopes from before a
// downgrade is internal/credlifecycle's problem, not this one; and anything at
// all during a window in which the comparison itself is failing, which is why
// tsm_authz_role_drift_last_check_timestamp_seconds is exported and must be
// alerted on alongside the counts.
func startAuthzDriftWatch(appDB, identityDB *sql.DB, interval time.Duration) func() {
	if interval <= 0 || appDB == nil || identityDB == nil {
		slog.Info("role-drift comparison disabled", "interval", interval)
		return func() {}
	}
	ctx, cancel := context.WithCancel(context.Background())
	telemetry.RegisterWorker(authzDriftWorker, interval)
	// ONCE, BEFORE THE FIRST TICK. Without it every tsm_authz_role_drift series is
	// ABSENT for a full interval after each restart — and permanently absent on a
	// crashlooping replica — so the alert the operator was told to write ("alert on
	// the counts AND on the age of the last check") has nothing to evaluate during
	// exactly the window a bad deploy is most likely to be discovered in.
	reportAuthzDrift(ctx, appDB, identityDB)
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				telemetry.WorkerTick(authzDriftWorker)
				reportAuthzDrift(ctx, appDB, identityDB)
			}
		}
	}()
	slog.Info("role-drift comparison started", "interval", interval)
	return func() {
		cancel()
		telemetry.UnregisterWorker(authzDriftWorker)
	}
}

// reportAuthzDrift runs one comparison and records it.
//
// Role DEFINITIONS are no longer compared at all: per-app definitions diverging
// from identity's copy is the intended end state of
// sethbacon/terraform-suite-identity#206, and the reads that fed that half of
// the report are retired. What remains is the membership comparison, and a
// disagreement there is always worth the ERROR line.
func reportAuthzDrift(ctx context.Context, appDB, identityDB *sql.DB) {
	// See runAuthzDrift: an app connection routed into identity compares that
	// schema against itself and reports agreement forever, so the detector would
	// go permanently, silently green on the one deployment that needs it.
	if _, _, err := approles.NewStore(appDB).Verify(ctx); err != nil {
		if ctx.Err() != nil {
			return
		}
		telemetry.AuthzDriftCheckFailed()
		slog.Error("role-drift comparison cannot run: this application's authorization tables did not verify", "error", err)
		return
	}
	res, err := approles.CheckDrift(ctx, appDB, identityDB)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		telemetry.AuthzDriftCheckFailed()
		slog.Error("role-drift comparison failed", "error", err)
		return
	}
	telemetry.AuthzDriftObserved(res.Compared, res.Missing, res.Stale, res.Mismatched)
	if res.AssignmentDrift() > 0 {
		slog.Error("role records disagree between this application's tables and the shared identity schema",
			"missing", res.Missing, "stale", res.Stale, "mismatched", res.Mismatched,
			"detail", res.String())
	}
}

// runAuthzDrift is the gate for sethbacon/terraform-suite-identity#206 Phase 3b.
//
// # What it is for
//
// The reads moved. Which role a principal holds in this application is now
// answered by organization_member_roles joined to this application's own
// role_templates, not by identity.organization_members joined to identity's. A
// gap between the two does not surface as an error: it surfaces as a user
// silently holding the wrong role — losing access they should have, or keeping
// access they should not — and nothing in the request path reports it.
//
// So the flip is gated on this command rather than on a release note. RUN IT
// AGAINST A DEPLOYMENT BEFORE UPGRADING IT ONTO A BUILD WHOSE
// TSM_AUTHZ_ROLE_SOURCE DEFAULTS TO "app", and require a zero exit. The binary
// need not be the one that is running: this connects to the two databases and
// reads them, so the new build's `authz-drift` answers for the old build's data,
// which is exactly the order an upgrade happens in.
//
// # Non-zero while anything is unreconciled
//
// The estate's precedent is `bind-secrets verify` and `rekey-targets verify`: an
// exit code a runbook step can gate on, not a report a human has to read
// correctly. It is non-zero for ANY disagreement, including a role definition
// whose scopes differ between the two schemas — the case Phase 3a's DriftQuery
// could not see, and the one that after the flip means the right role name
// granting the wrong permissions.
//
// # What a non-zero exit means, and what to do
//
// Restart the backend. approles.Reconcile restates every assignment from
// identity and sweeps what identity no longer has, and now logs what it changed.
// Then run this again. Drift that SURVIVES a restart is the report worth acting
// on: it means the reconcile is failing rather than that a single mirror write
// slipped, and the startup log will say why.
// runReownRoots reports ownership across the partition roots, or moves it.
//
// An empty from/to is the VERIFY mode. It is the default reading of the command
// because it is the read-only one -- the writing mode has to be asked for by
// name and with both organizations spelled out.
func runReownRoots(cfg *config.Config, from, to string) error {
	database, err := db.Connect(cfg.Database.GetDSN(), cfg.Database.MaxConnections, cfg.Database.MinIdleConnections)
	if err != nil {
		return fmt.Errorf("failed to connect to database: %w", err)
	}
	defer func() { _ = database.Close() }()

	ctx := context.Background()
	if from == "" && to == "" {
		res, err := maintenance.Census(ctx, database)
		slog.Info("reown-roots census", "ownership", res.String())
		return err
	}
	// The destination is confirmed against the IDENTITY schema, on its own
	// connection and search_path. organizations live there, and 000033 gives the
	// partition no foreign key into it precisely because identity may be a
	// different database — so the application connection is not the place to ask.
	identityDB, err := db.Connect(
		cfg.IdentityDatabase.GetDSNWithSearchPath("identity,public"),
		cfg.IdentityDatabase.MaxConnections, cfg.IdentityDatabase.MinIdleConnections,
	)
	if err != nil {
		return fmt.Errorf("reown-roots needs the identity schema to confirm the destination organization: %w", err)
	}
	defer func() { _ = identityDB.Close() }()
	// approles.NewMembers, not idstore.NewOrganizationRepository directly: a
	// repository obtained that way writes identity without writing this
	// application's own organization_member_roles, and a class guard fails the
	// build on it. The read below is promoted unchanged through the embedded
	// repository, so this costs nothing and keeps the one construction path.
	orgs := approles.NewMembers(identityDB, database, approles.RoleSource(cfg.Authz.RoleSource))

	res, err := maintenance.Move(ctx, database, from, to, func(ctx context.Context, id string) (bool, error) {
		org, err := orgs.GetByID(ctx, id, idstore.OrgScopeAllOrganizations())
		if errors.Is(err, idstore.ErrNotFound) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		return org != nil, nil
	})
	if err != nil {
		return err
	}
	slog.Info("reown-roots complete", "from", from, "to", to, "result", res.String())
	return nil
}

func runAuthzDrift(cfg *config.Config) error {
	telemetry.SetupLogger(cfg.Logging.Format, cfg.Logging.Level)

	database, err := db.Connect(cfg.Database.GetDSN(), cfg.Database.MaxConnections, cfg.Database.MinIdleConnections)
	if err != nil {
		return fmt.Errorf("failed to connect to database: %w", err)
	}
	defer func() { _ = database.Close() }()

	// The SAME search_path the server gives each pool. The comparison reads
	// unqualified names on both connections, so a pool built differently here
	// would compare a different pair of tables than the one the server reads —
	// and would do it silently, reporting agreement between two tables nothing
	// authorizes from.
	identityDB, err := db.Connect(
		cfg.IdentityDatabase.GetDSNWithSearchPath("identity,public"),
		cfg.IdentityDatabase.MaxConnections, cfg.IdentityDatabase.MinIdleConnections,
	)
	if err != nil {
		return fmt.Errorf("failed to connect to identity schema: %w", err)
	}
	defer func() { _ = identityDB.Close() }()

	// ROUTING FIRST, OR THE GATE CAN REPORT A FALSE CLEAN.
	//
	// Both sides of the comparison name their tables UNQUALIFIED, placed by each
	// connection's search_path. On an app connection that resolves the identity
	// schema — the exact misconfiguration migration 000032's pre-check and
	// Store.Verify exist to refuse — this would read identity's tables twice,
	// find them identical to themselves, and exit zero. A gate that certifies the
	// one configuration the whole phase is designed to prevent is worse than no
	// gate: it is the "green class gate certified a live finding" shape, and it
	// would do it on the deployment least ready for the flip.
	//
	// Reconcile does this already, before its own CheckDrift; the two callers that
	// do not are this command and the periodic watch, so both check here.
	if _, _, err := approles.NewStore(database).Verify(context.Background()); err != nil {
		return fmt.Errorf("authz-drift: %w", err)
	}

	res, err := approles.CheckDrift(context.Background(), database, identityDB)
	if err != nil {
		return fmt.Errorf("authz-drift: %w", err)
	}

	// The group-mapping half (terraform-suite-identity#206 phase 2, migration
	// 000036) rides the same verb: it compares the same two connections, it
	// gates the same program's next step, and a deployment that would run one
	// check and not the other is exactly how half a dual-write ships. Both
	// halves run before either is judged, so one dirty half never hides the
	// other's report.
	gmRes, err := repositories.CheckGroupMappingDrift(context.Background(), identityDB, database)
	if err != nil {
		return fmt.Errorf("authz-drift: could not compare the group-mapping copies: %w", err)
	}

	if res.Clean() && gmRes.Clean() {
		slog.Info("authz-drift clean", "roles", res.String(), "group_mappings", gmRes.String())
		return nil
	}
	if !res.Clean() {
		slog.Error("authz-drift found this application's role tables and the shared identity schema in disagreement",
			"result", res.String())
	}
	if !gmRes.Clean() {
		for _, row := range gmRes.Rows {
			slog.Error("group-mapping drift", "row", row.String())
		}
		slog.Error("authz-drift found this application's group_mappings table and the sso_settings overlay in disagreement",
			"result", gmRes.String(),
			"repair", "restarting the backend re-derives group_mappings from the overlay; drift that survives a restart means the reconcile is failing and the startup log says why")
	}
	return fmt.Errorf("authz-drift: %d role record(s) and %d group mapping(s) disagree; "+
		"do not switch authorization or group-mapping reads onto this application's tables until this reports zero",
		res.AssignmentDrift(), len(gmRes.Rows))
}

func runBindTargets(cfg *config.Config, verify bool) error {
	database, err := db.Connect(cfg.Database.GetDSN(), cfg.Database.MaxConnections, cfg.Database.MinIdleConnections)
	if err != nil {
		return fmt.Errorf("failed to connect to database: %w", err)
	}
	defer func() { _ = database.Close() }()

	cipher, err := api.BuildIdentityTokenCipher()
	if err != nil {
		return fmt.Errorf("bind-targets needs the encryption key the server uses: %w", err)
	}

	res, err := maintenance.BindChannelTargets(context.Background(), database, cipher, verify)
	mode := "convert"
	if verify {
		mode = "verify"
	}
	slog.Info("bind-targets complete", "mode", mode, "result", res.String())
	return err
}

// runRekeyTargets re-encrypts notification-channel targets under the current
// TSM_ENCRYPTION_KEY, or with "verify" reports what still requires
// TSM_ENCRYPTION_KEY_PREVIOUS without writing anything.
//
// This is what finishes a key rotation. bind-targets deliberately skips a row it
// can already open under its own context, and that open falls back to the
// previous key — so once targets are row-bound, bind-targets re-encrypts nothing
// and the previous key can never be retired (#364).
//
// The verify form is the exit criterion, and the reason it takes the key rather
// than only the cipher: a cipher with a fallback cannot tell "opened with the
// current key" from "opened with the previous one", so it cannot answer the
// question the gate asks. It exits non-zero while any row still needs the
// previous key, which is what a runbook step gates the deletion on.
func runRekeyTargets(cfg *config.Config, verify bool) error {
	database, err := db.Connect(cfg.Database.GetDSN(), cfg.Database.MaxConnections, cfg.Database.MinIdleConnections)
	if err != nil {
		return fmt.Errorf("failed to connect to database: %w", err)
	}
	defer func() { _ = database.Close() }()

	cipher, err := api.BuildIdentityTokenCipher()
	if err != nil {
		return fmt.Errorf("rekey-targets needs the encryption key the server uses: %w", err)
	}
	currentKey, err := api.CurrentTSMEncryptionKey()
	if err != nil {
		return fmt.Errorf("rekey-targets needs the encryption key the server uses: %w", err)
	}

	res, err := maintenance.RekeyChannelTargets(context.Background(), database, cipher, currentKey, verify)
	mode := "re-encrypt"
	if verify {
		mode = "verify"
	}
	slog.Info("rekey-targets complete", "mode", mode, "result", res.String())
	return err
}
