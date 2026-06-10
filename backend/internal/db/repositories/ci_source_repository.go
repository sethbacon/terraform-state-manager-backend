package repositories

import (
	"context"
	"database/sql"
	"errors"
)

// CISource is an org-level CI provider connection (ADO org/project or GitHub
// owner) whose credential is shared by the pipeline connections created from it.
type CISource struct {
	ID             string  `json:"id"`
	Name           string  `json:"name"`
	Provider       string  `json:"provider"`
	Organization   string  `json:"organization"`
	Project        *string `json:"project,omitempty"`
	EncryptedToken []byte  `json:"-"`
	CreatedAt      string  `json:"created_at"`
	UpdatedAt      string  `json:"updated_at"`
}

// CISourceRepository is the DAO for ci_sources.
type CISourceRepository struct {
	db *sql.DB
}

func NewCISourceRepository(db *sql.DB) *CISourceRepository {
	return &CISourceRepository{db: db}
}

const ciSourceColumns = `id, name, provider, organization, project, encrypted_token, created_at::text, updated_at::text`

func scanCISource(scanner interface{ Scan(dest ...any) error }) (*CISource, error) {
	var s CISource
	if err := scanner.Scan(&s.ID, &s.Name, &s.Provider, &s.Organization, &s.Project,
		&s.EncryptedToken, &s.CreatedAt, &s.UpdatedAt); err != nil {
		return nil, err
	}
	return &s, nil
}

func (r *CISourceRepository) List(ctx context.Context) ([]CISource, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT `+ciSourceColumns+` FROM ci_sources ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := make([]CISource, 0)
	for rows.Next() {
		s, err := scanCISource(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *s)
	}
	return out, rows.Err()
}

func (r *CISourceRepository) GetByID(ctx context.Context, id string) (*CISource, error) {
	s, err := scanCISource(r.db.QueryRowContext(ctx,
		`SELECT `+ciSourceColumns+` FROM ci_sources WHERE id = $1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return s, err
}

func (r *CISourceRepository) Create(ctx context.Context, s *CISource) (*CISource, error) {
	return scanCISource(r.db.QueryRowContext(ctx,
		`INSERT INTO ci_sources (name, provider, organization, project, encrypted_token)
		 VALUES ($1, $2, $3, $4, $5) RETURNING `+ciSourceColumns,
		s.Name, s.Provider, s.Organization, s.Project, s.EncryptedToken))
}

func (r *CISourceRepository) Delete(ctx context.Context, id string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM ci_sources WHERE id = $1`, id)
	return err
}
