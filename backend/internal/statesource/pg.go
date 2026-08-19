package statesource

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // PostgreSQL driver, already a project dependency
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

// pgKeywordPassword matches a `password=` keyword in a libpq keyword/value DSN
// (start-of-string or whitespace-delimited), case-insensitive.
var pgKeywordPassword = regexp.MustCompile(`(?i)(^|\s)password\s*=`)

// dsnHasPassword reports whether a PostgreSQL DSN embeds a password, in either
// the URL form (postgres://user:pass@host) or the libpq keyword form
// (host=... password=...).
func dsnHasPassword(dsn string) bool {
	if u, err := url.Parse(strings.TrimSpace(dsn)); err == nil && u.User != nil {
		if _, ok := u.User.Password(); ok {
			return true
		}
	}
	return pgKeywordPassword.MatchString(dsn)
}

func newPG(config, credentials map[string]any) (*pgSource, error) {
	connStr, _ := credentials["conn_str"].(string)
	if connStr == "" {
		// The config map is stored unencrypted and echoed in source-detail
		// responses, so only a PASSWORDLESS DSN (e.g. trust auth in a lab) may
		// come from it. A password-bearing DSN must go through credentials.conn_str
		// so it is encrypted at rest and never returned to the API/UI.
		if cfgConn, _ := config["conn_str"].(string); cfgConn != "" {
			if dsnHasPassword(cfgConn) {
				return nil, fmt.Errorf("pg source: config.conn_str contains a password; put a password-bearing DSN in credentials.conn_str (encrypted at rest) instead of config")
			}
			connStr = cfgConn
		}
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
// coverage:skip:requires-database
func (p *pgSource) open() (*sql.DB, error) {
	db, err := sql.Open("pgx", p.connStr)
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

// coverage:skip:requires-database
func (p *pgSource) List(ctx context.Context) ([]StateRef, error) {
	db, err := p.open()
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }()

	rows, err := db.QueryContext(ctx,
		fmt.Sprintf("SELECT name, octet_length(data), md5(data::text) FROM %s ORDER BY name", p.table())) // #nosec G201 -- schema validated as identifier in newPG
	if err != nil {
		return nil, fmt.Errorf("pg list failed: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var refs []StateRef
	for rows.Next() {
		var name string
		var size sql.NullInt64
		var hash sql.NullString
		if err := rows.Scan(&name, &size, &hash); err != nil {
			return nil, fmt.Errorf("pg list scan failed: %w", err)
		}
		refs = append(refs, StateRef{Key: name, Name: name, Size: size.Int64, Version: hash.String})
	}
	return refs, rows.Err()
}

// coverage:skip:requires-database
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
		return nil, fmt.Errorf("state %q %w", key, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("pg read failed: %w", err)
	}
	return &RawState{Key: key, Data: data, Size: int64(len(data))}, nil
}

// Write upserts the state row, matching the pg backend's name-keyed layout.
// coverage:skip:requires-database
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

// Delete removes the state row. A missing row is reported as ErrNotFound.
// coverage:skip:requires-database
func (p *pgSource) Delete(ctx context.Context, key string) error {
	db, err := p.open()
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	res, err := db.ExecContext(ctx,
		fmt.Sprintf("DELETE FROM %s WHERE name = $1", p.table()), key) // #nosec G201 -- schema validated as identifier in newPG
	if err != nil {
		return fmt.Errorf("pg delete failed: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("state %q %w", key, ErrNotFound)
	}
	return nil
}
