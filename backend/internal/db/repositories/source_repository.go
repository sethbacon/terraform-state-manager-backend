// Package repositories implements the data-access layer for the state manager's
// own (public-schema) tables. Identity data uses the shared identity module's
// repositories instead.
package repositories

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

// Source is a configured connection to a backend where Terraform state lives.
// Credentials (when needed) are stored encrypted separately; Config/Scope hold
// non-secret settings.
type Source struct {
	ID       string         `json:"id"`
	Name     string         `json:"name"`
	Type     string         `json:"type"`
	Endpoint string         `json:"endpoint"`
	Config   map[string]any `json:"config"`
	Scope    map[string]any `json:"scope"`
	// EncryptedCredentials holds the AES-GCM-sealed secret blob (never serialized
	// to API responses). Empty when the source needs no credentials.
	EncryptedCredentials []byte `json:"-"`
	CreatedAt            string `json:"created_at"`
	UpdatedAt            string `json:"updated_at"`
}

// SourceRepository is the DAO for the state_sources table.
type SourceRepository struct {
	db *sql.DB
}

// NewSourceRepository creates a SourceRepository over the app (public) connection.
func NewSourceRepository(db *sql.DB) *SourceRepository {
	return &SourceRepository{db: db}
}

const sourceColumns = `id, name, type, COALESCE(endpoint, ''), config, scope, encrypted_credentials, created_at::text, updated_at::text`

func scanSource(scanner interface {
	Scan(dest ...any) error
}) (*Source, error) {
	var s Source
	var configJSON, scopeJSON []byte
	if err := scanner.Scan(&s.ID, &s.Name, &s.Type, &s.Endpoint, &configJSON, &scopeJSON, &s.EncryptedCredentials, &s.CreatedAt, &s.UpdatedAt); err != nil {
		return nil, err
	}
	if len(configJSON) > 0 {
		_ = json.Unmarshal(configJSON, &s.Config)
	}
	if len(scopeJSON) > 0 {
		_ = json.Unmarshal(scopeJSON, &s.Scope)
	}
	return &s, nil
}

// List returns all configured sources, newest first.
func (r *SourceRepository) List(ctx context.Context) ([]Source, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT `+sourceColumns+` FROM state_sources ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	sources := []Source{}
	for rows.Next() {
		s, err := scanSource(rows)
		if err != nil {
			return nil, err
		}
		sources = append(sources, *s)
	}
	return sources, rows.Err()
}

// GetByID returns the source with the given id, or (nil, nil) if not found.
func (r *SourceRepository) GetByID(ctx context.Context, id string) (*Source, error) {
	row := r.db.QueryRowContext(ctx, `SELECT `+sourceColumns+` FROM state_sources WHERE id = $1`, id)
	s, err := scanSource(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return s, nil
}

// Create inserts a source and returns it with its generated id/timestamps.
func (r *SourceRepository) Create(ctx context.Context, s *Source) (*Source, error) {
	configJSON, err := json.Marshal(orEmptyMap(s.Config))
	if err != nil {
		return nil, err
	}
	scopeJSON, err := json.Marshal(orEmptyMap(s.Scope))
	if err != nil {
		return nil, err
	}
	var endpoint any
	if s.Endpoint != "" {
		endpoint = s.Endpoint
	}
	var creds any
	if len(s.EncryptedCredentials) > 0 {
		creds = s.EncryptedCredentials
	}
	row := r.db.QueryRowContext(ctx, `
		INSERT INTO state_sources (name, type, endpoint, config, scope, encrypted_credentials)
		VALUES ($1, $2, $3, $4::jsonb, $5::jsonb, $6)
		RETURNING `+sourceColumns,
		s.Name, s.Type, endpoint, string(configJSON), string(scopeJSON), creds)
	created, err := scanSource(row)
	if err != nil {
		return nil, fmt.Errorf("failed to create source: %w", err)
	}
	return created, nil
}

// Update replaces a source's name, endpoint, config, and scope. Credentials
// are replaced only when EncryptedCredentials is non-empty (blank keeps the
// stored secret). Type is immutable. Returns (nil, nil) if the id is unknown.
func (r *SourceRepository) Update(ctx context.Context, s *Source) (*Source, error) {
	configJSON, err := json.Marshal(orEmptyMap(s.Config))
	if err != nil {
		return nil, err
	}
	scopeJSON, err := json.Marshal(orEmptyMap(s.Scope))
	if err != nil {
		return nil, err
	}
	var endpoint any
	if s.Endpoint != "" {
		endpoint = s.Endpoint
	}
	row := r.db.QueryRowContext(ctx, `
		UPDATE state_sources SET
			name = $2,
			endpoint = $3,
			config = $4::jsonb,
			scope = $5::jsonb,
			encrypted_credentials = CASE WHEN $6::bytea IS NULL THEN encrypted_credentials ELSE $6 END,
			updated_at = now()
		WHERE id = $1
		RETURNING `+sourceColumns,
		s.ID, s.Name, endpoint, string(configJSON), string(scopeJSON), nullableBytes(s.EncryptedCredentials))
	updated, err := scanSource(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to update source: %w", err)
	}
	return updated, nil
}

func nullableBytes(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return b
}

// Delete removes a source by id.
func (r *SourceRepository) Delete(ctx context.Context, id string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM state_sources WHERE id = $1`, id)
	return err
}

func orEmptyMap(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}
