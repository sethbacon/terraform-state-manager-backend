// Package db manages database connections and schema migrations for the state
// manager. It wraps database/sql for connection pooling and golang-migrate for
// schema versioning. Migrations are embedded in the binary (via go:embed) so the
// server can apply schema changes on startup without external tooling.
package db

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"time"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	_ "github.com/jackc/pgx/v5/stdlib"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Connect establishes a pooled connection to PostgreSQL and verifies it.
// coverage:skip:requires-database
func Connect(dsn string, maxConnections, minIdleConnections int) (*sql.DB, error) {
	database, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	database.SetMaxOpenConns(maxConnections)
	database.SetMaxIdleConns(minIdleConnections)
	database.SetConnMaxLifetime(5 * time.Minute)
	database.SetConnMaxIdleTime(30 * time.Second)

	if err := database.Ping(); err != nil {
		return nil, fmt.Errorf("failed to ping database: %w", err)
	}
	return database, nil
}

// RunMigrations applies embedded migrations in the given direction ("up"/"down").
// coverage:skip:requires-database
func RunMigrations(database *sql.DB, direction string) error {
	m, err := newMigrator(context.Background(), database)
	if err != nil {
		return err
	}
	defer closeMigrator(m)

	switch direction {
	case "up":
		if err := m.Up(); err != nil && err != migrate.ErrNoChange {
			return fmt.Errorf("failed to run migrations: %w", err)
		}
	case "down":
		if err := m.Down(); err != nil && err != migrate.ErrNoChange {
			return fmt.Errorf("failed to rollback migrations: %w", err)
		}
	default:
		return fmt.Errorf("invalid migration direction: %s (must be 'up' or 'down')", direction)
	}
	return nil
}

// GetMigrationVersion returns the current schema version.
// coverage:skip:requires-database
func GetMigrationVersion(database *sql.DB) (version uint, dirty bool, err error) {
	m, err := newMigrator(context.Background(), database)
	if err != nil {
		return 0, false, err
	}
	defer closeMigrator(m)
	version, dirty, err = m.Version()
	if err != nil && err != migrate.ErrNilVersion {
		return 0, false, fmt.Errorf("failed to get migration version: %w", err)
	}
	return version, dirty, nil
}

// newMigrator builds a migrator over a single connection borrowed from database.
//
// Every migrator returned here MUST be handed to closeMigrator, which returns
// the borrowed connection to the pool. Callers keep ownership of the pool
// itself, which this package never closes.
//
// The borrowing is explicit for a reason. postgres.WithInstance(database, ...)
// checks out a dedicated connection and holds it for the driver's lifetime,
// and nothing here released it -- so each call permanently consumed one slot
// of the caller's MaxOpenConns. cmd/server/main.go calls RunMigrations AND
// GetMigrationVersion with the long-lived application pool at startup, so two
// slots were lost for the life of the process.
//
// The obvious fix is a trap: WithInstance also records the *sql.DB on the
// driver, so driver.Close() -- and therefore m.Close() -- closes the caller's
// shared pool. "defer m.Close()" would close the application pool at startup.
// postgres.WithConnection takes a connection this package obtained itself and
// leaves that field nil, so Close() releases exactly what was borrowed.
//
// Third independent copy of this defect in the suite: terraform-suite-identity
// fixed it in #139, terraform-registry-backend in #788, and this repo has its
// own copy of the same pattern -- which is why neither of those fixes reached
// it.
//
// coverage:skip:requires-database
func newMigrator(ctx context.Context, database *sql.DB) (*migrate.Migrate, error) {
	conn, err := database.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to acquire a connection for migrations: %w", err)
	}
	driver, err := postgres.WithConnection(ctx, conn, &postgres.Config{})
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("failed to create migration driver: %w", err)
	}
	sourceDriver, err := iofs.New(migrationsFS, "migrations")
	if err != nil {
		// driver.Close() releases the borrowed connection back to the pool (and
		// only that: WithConnection leaves the driver's db field nil).
		_ = driver.Close()
		return nil, fmt.Errorf("failed to create migration source: %w", err)
	}
	m, err := migrate.NewWithInstance("iofs", sourceDriver, "postgres", driver)
	if err != nil {
		_ = driver.Close()
		_ = sourceDriver.Close()
		return nil, fmt.Errorf("failed to create migration instance: %w", err)
	}
	return m, nil
}

// closeMigrator releases the migrator's source driver and returns its borrowed
// connection to the pool it came from. Best-effort by design: a close failure
// must not mask (or manufacture) an error from the migration itself, and the
// connection is released either way.
func closeMigrator(m *migrate.Migrate) {
	if m == nil {
		return
	}
	_, _ = m.Close()
}
