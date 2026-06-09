// Package db manages database connections and schema migrations for the state
// manager. It wraps database/sql for connection pooling and golang-migrate for
// schema versioning. Migrations are embedded in the binary (via go:embed) so the
// server can apply schema changes on startup without external tooling.
package db

import (
	"database/sql"
	"embed"
	"fmt"
	"time"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	_ "github.com/lib/pq"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Connect establishes a pooled connection to PostgreSQL and verifies it.
func Connect(dsn string, maxConnections, minIdleConnections int) (*sql.DB, error) {
	database, err := sql.Open("postgres", dsn)
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
func RunMigrations(database *sql.DB, direction string) error {
	m, err := newMigrator(database)
	if err != nil {
		return err
	}

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
func GetMigrationVersion(database *sql.DB) (version uint, dirty bool, err error) {
	m, err := newMigrator(database)
	if err != nil {
		return 0, false, err
	}
	version, dirty, err = m.Version()
	if err != nil && err != migrate.ErrNilVersion {
		return 0, false, fmt.Errorf("failed to get migration version: %w", err)
	}
	return version, dirty, nil
}

func newMigrator(database *sql.DB) (*migrate.Migrate, error) {
	driver, err := postgres.WithInstance(database, &postgres.Config{})
	if err != nil {
		return nil, fmt.Errorf("failed to create migration driver: %w", err)
	}
	sourceDriver, err := iofs.New(migrationsFS, "migrations")
	if err != nil {
		return nil, fmt.Errorf("failed to create migration source: %w", err)
	}
	m, err := migrate.NewWithInstance("iofs", sourceDriver, "postgres", driver)
	if err != nil {
		return nil, fmt.Errorf("failed to create migration instance: %w", err)
	}
	return m, nil
}
