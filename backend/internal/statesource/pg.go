package statesource

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"time"

	_ "github.com/lib/pq" // PostgreSQL driver, already a project dependency
)

// pgSchemaName restricts schema_name to a plain SQL identifier. The schema name
// is interpolated into queries (placeholders cannot parameterize identifiers), so
// anything outside [A-Za-z_][A-Za-z0-9_]* is rejected up front — closing the SQL
// injection the original scanner had.
var pgSchemaName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,62}$`)

// pgSource reads Terraform state stored by the pg backend (a "states" table with
// name + data columns inside a schema, default "terraform_remote_state"). The
// connection string is a credential (it usually embeds a password).
type pgSource struct {
	connStr string
	schema  string
}

func newPG(config, credentials map[string]any) (*pgSource, error) {
	connStr, _ := credentials["conn_str"].(string)
	if connStr == "" {
		// Allow non-secret DSNs (e.g. trust auth in a lab) via config.
		connStr, _ = config["conn_str"].(string)
	}
	if connStr == "" {
		return nil, fmt.Errorf("pg source requires credentials.conn_str (PostgreSQL DSN)")
	}
	schema, _ := config["schema_name"].(string)
	if schema == "" {
		schema = "terraform_remote_state"
	}
	if !pgSchemaName.MatchString(schema) {
		return nil, fmt.Errorf("invalid schema_name %q (must be a plain SQL identifier)", schema)
	}
	return &pgSource{connStr: connStr, schema: schema}, nil
}

// open returns a short-lived connection; this connector is a scanner, not a pool.
func (p *pgSource) open() (*sql.DB, error) {
	db, err := sql.Open("postgres", p.connStr)
	if err != nil {
		return nil, fmt.Errorf("pg open failed: %w", err)
	}
	db.SetMaxOpenConns(2)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(time.Minute)
	return db, nil
}

// table returns the schema-qualified states table. The schema is validated by
// pgSchemaName in newPG, so interpolation here is safe.
func (p *pgSource) table() string {
	return fmt.Sprintf("%q.states", p.schema)
}

func (p *pgSource) List(ctx context.Context) ([]StateRef, error) {
	db, err := p.open()
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }()

	rows, err := db.QueryContext(ctx,
		fmt.Sprintf("SELECT name, octet_length(data) FROM %s ORDER BY name", p.table())) // #nosec G201 -- schema validated as identifier in newPG
	if err != nil {
		return nil, fmt.Errorf("pg list failed: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var refs []StateRef
	for rows.Next() {
		var name string
		var size sql.NullInt64
		if err := rows.Scan(&name, &size); err != nil {
			return nil, fmt.Errorf("pg list scan failed: %w", err)
		}
		refs = append(refs, StateRef{Key: name, Name: name, Size: size.Int64})
	}
	return refs, rows.Err()
}

func (p *pgSource) Read(ctx context.Context, key string) (*RawState, error) {
	db, err := p.open()
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }()

	var data []byte
	err = db.QueryRowContext(ctx,
		fmt.Sprintf("SELECT data FROM %s WHERE name = $1", p.table()), key).Scan(&data) // #nosec G201 -- schema validated as identifier in newPG
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("state %q not found", key)
	}
	if err != nil {
		return nil, fmt.Errorf("pg read failed: %w", err)
	}
	return &RawState{Key: key, Data: data, Size: int64(len(data))}, nil
}

// Write upserts the state row, matching the pg backend's name-keyed layout.
func (p *pgSource) Write(ctx context.Context, key string, data []byte) error {
	db, err := p.open()
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	_, err = db.ExecContext(ctx,
		fmt.Sprintf(`INSERT INTO %s (name, data) VALUES ($1, $2)
			ON CONFLICT (name) DO UPDATE SET data = EXCLUDED.data`, p.table()), // #nosec G201 -- schema validated as identifier in newPG
		key, string(data))
	if err != nil {
		return fmt.Errorf("pg write failed: %w", err)
	}
	return nil
}
