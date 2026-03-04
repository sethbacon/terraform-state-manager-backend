// Package pg implements a Terraform state file scanner for the PostgreSQL
// backend.
//
// Terraform's pg backend stores state in a table (by default
// "terraform_remote_state.states") with columns id, name, and data.  This
// client queries that table directly using database/sql with the lib/pq driver
// which is already a dependency of the project.
package pg

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	// PostgreSQL driver -- already used by the rest of the project.
	_ "github.com/lib/pq"

	"github.com/terraform-state-manager/terraform-state-manager/internal/clients/azure"
)

// Config holds the parameters needed to connect to the Terraform pg backend
// database.
type Config struct {
	// ConnStr is the PostgreSQL connection string (DSN).
	ConnStr string `json:"conn_str"`
	// SchemaName overrides the default schema name ("terraform_remote_state").
	SchemaName string `json:"schema_name,omitempty"`
}

// Client reads Terraform state files from a PostgreSQL database.
type Client struct {
	config Config
}

// NewClient validates the supplied configuration and returns a ready-to-use
// Client.
func NewClient(cfg Config) (*Client, error) {
	if cfg.ConnStr == "" {
		return nil, fmt.Errorf("pg: conn_str is required")
	}

	if cfg.SchemaName == "" {
		cfg.SchemaName = "terraform_remote_state"
	}

	return &Client{
		config: cfg,
	}, nil
}

// schemaTable returns the fully-qualified table name
// (schema.states).
func (c *Client) schemaTable() string {
	return fmt.Sprintf("%s.states", c.config.SchemaName)
}

// openDB opens a short-lived connection to the configured PostgreSQL server.
// The caller is responsible for closing the returned *sql.DB.
func (c *Client) openDB() (*sql.DB, error) {
	db, err := sql.Open("postgres", c.config.ConnStr)
	if err != nil {
		return nil, fmt.Errorf("pg: failed to open database: %w", err)
	}
	// Keep connections short-lived -- this client is a scanner, not a
	// long-running service.
	db.SetMaxOpenConns(2)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(1 * time.Minute)
	return db, nil
}

// TestConnection verifies that the configured database is reachable and the
// expected schema/table exists.
func (c *Client) TestConnection(ctx context.Context) error {
	db, err := c.openDB()
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("pg: database ping failed: %w", err)
	}

	// Verify the states table exists.
	query := "SELECT 1 FROM information_schema.tables WHERE table_schema = $1 AND table_name = 'states' LIMIT 1"
	var one int
	err = db.QueryRowContext(ctx, query, c.config.SchemaName).Scan(&one)
	if err == sql.ErrNoRows {
		return fmt.Errorf("pg: table %s does not exist", c.schemaTable())
	}
	if err != nil {
		return fmt.Errorf("pg: failed to check table existence: %w", err)
	}

	return nil
}

// ListStateFiles queries the states table and returns a reference for each
// stored state.  The returned StateFileRef.Key corresponds to the "name"
// column.
func (c *Client) ListStateFiles(ctx context.Context) ([]azure.StateFileRef, error) {
	db, err := c.openDB()
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }()

	// The standard Terraform pg backend schema uses columns: id, name, data.
	// Some installations add an updated_at column; we probe for it and fall
	// back to a default timestamp.
	hasUpdatedAt, err := c.columnExists(ctx, db, "updated_at")
	if err != nil {
		return nil, err
	}

	var query string
	if hasUpdatedAt {
		query = fmt.Sprintf(
			"SELECT name, octet_length(data), updated_at FROM %s ORDER BY name",
			c.schemaTable(),
		)
	} else {
		query = fmt.Sprintf(
			"SELECT name, octet_length(data) FROM %s ORDER BY name",
			c.schemaTable(),
		)
	}

	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("pg: failed to list state files: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var refs []azure.StateFileRef
	for rows.Next() {
		var (
			name string
			size int64
		)

		if hasUpdatedAt {
			var updatedAt time.Time
			if err := rows.Scan(&name, &size, &updatedAt); err != nil {
				return nil, fmt.Errorf("pg: failed to scan row: %w", err)
			}
			refs = append(refs, azure.StateFileRef{
				Key:          name,
				LastModified: updatedAt,
				Size:         size,
			})
		} else {
			if err := rows.Scan(&name, &size); err != nil {
				return nil, fmt.Errorf("pg: failed to scan row: %w", err)
			}
			refs = append(refs, azure.StateFileRef{
				Key:          name,
				LastModified: time.Time{},
				Size:         size,
			})
		}
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pg: row iteration error: %w", err)
	}

	return refs, nil
}

// DownloadState retrieves the raw state data for the entry identified by
// ref.Key (the "name" column).
func (c *Client) DownloadState(ctx context.Context, ref azure.StateFileRef) ([]byte, error) {
	db, err := c.openDB()
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }()

	query := fmt.Sprintf("SELECT data FROM %s WHERE name = $1", c.schemaTable())
	var data []byte
	err = db.QueryRowContext(ctx, query, ref.Key).Scan(&data)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("pg: state file %q not found", ref.Key)
	}
	if err != nil {
		return nil, fmt.Errorf("pg: failed to download state %q: %w", ref.Key, err)
	}

	return data, nil
}

// columnExists checks whether a given column exists in the states table.
func (c *Client) columnExists(ctx context.Context, db *sql.DB, column string) (bool, error) {
	query := `
		SELECT 1
		FROM information_schema.columns
		WHERE table_schema = $1
		  AND table_name = 'states'
		  AND column_name = $2
		LIMIT 1`

	var one int
	err := db.QueryRowContext(ctx, query, c.config.SchemaName, column).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("pg: failed to check column %q: %w", column, err)
	}
	return true, nil
}
