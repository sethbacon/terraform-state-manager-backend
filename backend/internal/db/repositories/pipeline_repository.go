package repositories

import (
	"context"
	"database/sql"
	"encoding/json"
)

// PipelineConnection is a CI integration used to dispatch drift/version runs.
type PipelineConnection struct {
	ID             string         `json:"id"`
	Name           string         `json:"name"`
	Provider       string         `json:"provider"`
	Config         map[string]any `json:"config"`
	EncryptedToken []byte         `json:"-"`
	CreatedAt      string         `json:"created_at"`
	UpdatedAt      string         `json:"updated_at"`
}

// PipelineRepository is the DAO for pipeline_connections.
type PipelineRepository struct {
	db *sql.DB
}

func NewPipelineRepository(db *sql.DB) *PipelineRepository {
	return &PipelineRepository{db: db}
}

const pipelineColumns = `id, name, provider, config, encrypted_token, created_at::text, updated_at::text`

func scanPipeline(scanner interface{ Scan(dest ...any) error }) (*PipelineConnection, error) {
	var p PipelineConnection
	var configJSON []byte
	if err := scanner.Scan(&p.ID, &p.Name, &p.Provider, &configJSON, &p.EncryptedToken, &p.CreatedAt, &p.UpdatedAt); err != nil {
		return nil, err
	}
	if len(configJSON) > 0 {
		_ = json.Unmarshal(configJSON, &p.Config)
	}
	return &p, nil
}

func (r *PipelineRepository) List(ctx context.Context) ([]PipelineConnection, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT `+pipelineColumns+` FROM pipeline_connections ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PipelineConnection{}
	for rows.Next() {
		p, err := scanPipeline(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

func (r *PipelineRepository) GetByID(ctx context.Context, id string) (*PipelineConnection, error) {
	row := r.db.QueryRowContext(ctx, `SELECT `+pipelineColumns+` FROM pipeline_connections WHERE id = $1`, id)
	p, err := scanPipeline(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return p, nil
}

func (r *PipelineRepository) Create(ctx context.Context, p *PipelineConnection) (*PipelineConnection, error) {
	configJSON, err := json.Marshal(orEmptyMap(p.Config))
	if err != nil {
		return nil, err
	}
	var token any
	if len(p.EncryptedToken) > 0 {
		token = p.EncryptedToken
	}
	row := r.db.QueryRowContext(ctx, `
		INSERT INTO pipeline_connections (name, provider, config, encrypted_token)
		VALUES ($1, $2, $3::jsonb, $4)
		RETURNING `+pipelineColumns,
		p.Name, p.Provider, string(configJSON), token)
	return scanPipeline(row)
}

// Update edits a connection's name and config. The provider is immutable. The
// stored token is replaced only when updateToken is true (callers pass false to
// preserve the existing credential). Returns (nil, nil) when no row matches.
func (r *PipelineRepository) Update(ctx context.Context, p *PipelineConnection, updateToken bool) (*PipelineConnection, error) {
	configJSON, err := json.Marshal(orEmptyMap(p.Config))
	if err != nil {
		return nil, err
	}
	var token any
	if len(p.EncryptedToken) > 0 {
		token = p.EncryptedToken
	}
	row := r.db.QueryRowContext(ctx, `
		UPDATE pipeline_connections
		SET name = $2,
		    config = $3::jsonb,
		    encrypted_token = CASE WHEN $4 THEN $5 ELSE encrypted_token END,
		    updated_at = now()
		WHERE id = $1
		RETURNING `+pipelineColumns,
		p.ID, p.Name, string(configJSON), updateToken, token)
	updated, err := scanPipeline(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return updated, nil
}

func (r *PipelineRepository) Delete(ctx context.Context, id string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM pipeline_connections WHERE id = $1`, id)
	return err
}
