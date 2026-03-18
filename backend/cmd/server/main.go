// @title           Terraform State Manager API
// @version         1.0.0
// @description     REST API for the Terraform State Manager backend.
// @basePath        /api/v1
// @schemes         http https
//
// @securityDefinitions.apiKey  BearerAuth
// @in                          header
// @name                        Authorization
// @description                 Bearer JWT token. Format: "Bearer {token}"
//
// @securityDefinitions.apiKey  ApiKeyAuth
// @in                          header
// @name                        X-API-Key
// @description                 API key for service-to-service authentication

// Package main is the entry point for the Terraform State Manager server binary.
// It dispatches three subcommands — serve, migrate, and version — via os.Args.
package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/terraform-state-manager/terraform-state-manager/internal/api"
	"github.com/terraform-state-manager/terraform-state-manager/internal/auth"
	"github.com/terraform-state-manager/terraform-state-manager/internal/config"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
	"github.com/terraform-state-manager/terraform-state-manager/internal/telemetry"
	"golang.org/x/crypto/bcrypt"
)

const version = "0.1.0"

func main() {
	if err := run(); err != nil {
		log.Fatalf("fatal error: %v", err)
	}
}

func run() error {
	command := "serve"
	if len(os.Args) > 1 {
		command = os.Args[1]
	}

	configPath := os.Getenv("CONFIG_PATH")
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("failed to load configuration: %w", err)
	}

	switch command {
	case "serve":
		return serve(cfg)
	case "migrate":
		if len(os.Args) < 3 {
			return fmt.Errorf("usage: %s migrate <up|down>", os.Args[0])
		}
		return runMigrations(cfg, os.Args[2])
	case "version":
		fmt.Printf("Terraform State Manager v%s\n", version)
		return nil
	default:
		return fmt.Errorf("unknown command: %s (available: serve, migrate, version)", command)
	}
}

func serve(cfg *config.Config) error {
	// Setup structured logging early.
	telemetry.SetupLogger(cfg.Logging.Format, cfg.Logging.Level)

	slog.Info("starting Terraform State Manager",
		"version", version,
		"host", cfg.Server.Host,
		"port", cfg.Server.Port,
	)

	// Set Gin mode based on log level.
	if strings.ToLower(cfg.Logging.Level) == "debug" {
		gin.SetMode(gin.DebugMode)
	} else {
		gin.SetMode(gin.ReleaseMode)
	}

	// Validate JWT secret (fails in production if not set).
	if err := auth.ValidateJWTSecret(); err != nil {
		return fmt.Errorf("security configuration error: %w", err)
	}
	slog.Info("JWT secret validated")

	// Connect to database.
	database, err := db.Connect(
		cfg.Database.GetDSN(),
		cfg.Database.MaxConnections,
		cfg.Database.MinIdleConnections,
	)
	if err != nil {
		return fmt.Errorf("failed to connect to database: %w", err)
	}
	defer func() { _ = database.Close() }()

	slog.Info("connected to database",
		"host", cfg.Database.Host,
		"port", cfg.Database.Port,
		"name", cfg.Database.Name,
	)

	// Start Prometheus DB stats collector.
	telemetry.StartDBStatsCollector(database)

	// Run auto-migrations on startup.
	slog.Info("running database migrations")
	if err := db.RunMigrations(database, "up"); err != nil {
		return fmt.Errorf("failed to run auto-migrations: %w", err)
	}
	slog.Info("database migrations completed")

	// Log migration version.
	migrationVersion, dirty, err := db.GetMigrationVersion(database)
	if err != nil {
		slog.Warn("could not determine migration version", "error", err)
	} else {
		slog.Info("database migration status", "version", migrationVersion, "dirty", dirty)
	}

	// Handle setup token generation.
	sqlxDB := sqlx.NewDb(database, "postgres")
	oidcConfigRepo := repositories.NewOIDCConfigRepository(sqlxDB)
	if err := handleSetupToken(oidcConfigRepo); err != nil {
		slog.Warn("setup token handling failed", "error", err)
	}

	// Start Prometheus metrics server on dedicated port if enabled.
	if cfg.Telemetry.Enabled && cfg.Telemetry.Metrics.Enabled {
		metricsAddr := fmt.Sprintf(":%d", cfg.Telemetry.Metrics.PrometheusPort)
		metricsMux := http.NewServeMux()
		metricsMux.Handle("/metrics", promhttp.Handler())

		metricsServer := &http.Server{
			Addr:         metricsAddr,
			Handler:      metricsMux,
			ReadTimeout:  10 * time.Second,
			WriteTimeout: 10 * time.Second,
		}

		go func() {
			slog.Info("starting metrics server", "addr", metricsAddr)
			if err := metricsServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				slog.Error("metrics server failed", "error", err)
			}
		}()
	}

	// Start pprof server if enabled.
	if cfg.Telemetry.Profiling.Enabled {
		pprofAddr := fmt.Sprintf(":%d", cfg.Telemetry.Profiling.Port)
		go func() {
			slog.Info("starting pprof server", "addr", pprofAddr)
			pprofServer := &http.Server{
				Addr:         pprofAddr,
				Handler:      http.DefaultServeMux,
				ReadTimeout:  30 * time.Second,
				WriteTimeout: 30 * time.Second,
			}
			if err := pprofServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				slog.Error("pprof server failed", "error", err)
			}
		}()
	}

	// Create API router.
	router, bgServices := api.NewRouter(cfg, database)

	// Create HTTP server with configured timeouts.
	addr := cfg.Server.GetAddress()
	server := &http.Server{
		Addr:         addr,
		Handler:      router,
		ReadTimeout:  cfg.Server.ReadTimeout,
		WriteTimeout: cfg.Server.WriteTimeout,
		IdleTimeout:  120 * time.Second,
	}

	// Start server goroutine.
	serverErrors := make(chan error, 1)
	go func() {
		if cfg.Security.TLS.Enabled {
			slog.Info("starting HTTPS server", "addr", addr)
			serverErrors <- server.ListenAndServeTLS(
				cfg.Security.TLS.CertFile,
				cfg.Security.TLS.KeyFile,
			)
		} else {
			slog.Info("starting HTTP server", "addr", addr)
			serverErrors <- server.ListenAndServe()
		}
	}()

	// Wait for interrupt signal or server error.
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-serverErrors:
		if err != nil && err != http.ErrServerClosed {
			return fmt.Errorf("server error: %w", err)
		}
	case sig := <-quit:
		slog.Info("received shutdown signal", "signal", sig)
	}

	// Graceful shutdown with 10s timeout.
	slog.Info("shutting down server gracefully")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := server.Shutdown(ctx); err != nil {
		return fmt.Errorf("server forced to shutdown: %w", err)
	}

	// Stop background services (rate limiters, etc.).
	bgServices.Shutdown()

	slog.Info("server shutdown complete")
	return nil
}

// handleSetupToken generates a one-time setup token on first boot. The raw
// token is printed to logs; only the bcrypt hash is persisted in the database.
func handleSetupToken(repo *repositories.OIDCConfigRepository) error {
	ctx := context.Background()

	completed, err := repo.IsSetupCompleted(ctx)
	if err != nil {
		return fmt.Errorf("failed to check setup status: %w", err)
	}
	if completed {
		slog.Debug("initial setup already completed")
		return nil
	}

	existingHash, err := repo.GetSetupTokenHash(ctx)
	if err != nil {
		return fmt.Errorf("failed to get existing setup token hash: %w", err)
	}
	if existingHash != "" {
		slog.Info("setup token already generated — check logs from first boot or SETUP_TOKEN_FILE")
		return nil
	}

	// Generate the token: 32 random bytes, base64url-encoded with tsm_setup_ prefix.
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		return fmt.Errorf("failed to generate random bytes: %w", err)
	}
	token := "tsm_setup_" + base64.RawURLEncoding.EncodeToString(tokenBytes)

	// Hash with bcrypt before storing.
	hashedToken, err := bcrypt.GenerateFromPassword([]byte(token), 12)
	if err != nil {
		return fmt.Errorf("failed to hash setup token: %w", err)
	}

	if err := repo.SetSetupTokenHash(ctx, string(hashedToken)); err != nil {
		return fmt.Errorf("failed to store setup token hash: %w", err)
	}

	// Print prominently.
	separator := strings.Repeat("=", 66)
	slog.Info("")
	slog.Info(separator)
	slog.Info("  INITIAL SETUP REQUIRED")
	slog.Info("")
	slog.Info("  Setup Token: " + token)
	slog.Info("")
	slog.Info("  Use this token to complete initial setup via:")
	slog.Info("    Browser:  Navigate to https://<your-host>/setup")
	slog.Info("    API:      POST /api/v1/setup/validate-token")
	slog.Info("              Authorization: SetupToken <token>")
	slog.Info("")
	slog.Info("  This token is single-use and will be invalidated after setup.")
	slog.Info(separator)
	slog.Info("")

	// Optionally write the token to a file (for container secret mounting).
	if tokenFile := os.Getenv("SETUP_TOKEN_FILE"); tokenFile != "" {
		if strings.Contains(filepath.ToSlash(tokenFile), "..") {
			slog.Warn("SETUP_TOKEN_FILE contains path-traversal sequences, ignoring", "path", tokenFile)
		} else {
			cleanPath := filepath.Clean(tokenFile)
			if err := os.MkdirAll(filepath.Dir(cleanPath), 0o700); err != nil {
				slog.Warn("failed to create directory for setup token file", "path", cleanPath, "error", err)
			} else if err := os.WriteFile(cleanPath, []byte(token), 0o600); err != nil {
				slog.Warn("failed to write setup token to file", "path", cleanPath, "error", err)
			} else {
				slog.Info("setup token written to file", "path", cleanPath)
			}
		}
	}

	return nil
}

func runMigrations(cfg *config.Config, direction string) error {
	telemetry.SetupLogger(cfg.Logging.Format, cfg.Logging.Level)

	slog.Info("running database migrations", "direction", direction)

	database, err := db.Connect(
		cfg.Database.GetDSN(),
		cfg.Database.MaxConnections,
		cfg.Database.MinIdleConnections,
	)
	if err != nil {
		return fmt.Errorf("failed to connect to database: %w", err)
	}
	defer func() { _ = database.Close() }()

	if err := db.RunMigrations(database, direction); err != nil {
		return fmt.Errorf("migration %s failed: %w", direction, err)
	}

	ver, dirty, err := db.GetMigrationVersion(database)
	if err != nil {
		slog.Warn("could not determine migration version", "error", err)
	} else {
		slog.Info("migration completed", "version", ver, "dirty", dirty)
	}

	return nil
}
