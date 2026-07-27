package repositories

import (
	"context"
	"database/sql"
	"errors"
)

// WorkflowTemplate is an operator-managed CI workflow template, keyed by
// (provider, kind, profile). Built-ins are seeded with profile "default" and
// IsBuiltin=true; operators may add/edit other profiles to fit their repos.
type WorkflowTemplate struct {
	ID          string `json:"id"`
	Provider    string `json:"provider"`
	Kind        string `json:"kind"`
	Profile     string `json:"profile"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Content     string `json:"content"`
	IsBuiltin   bool   `json:"is_builtin"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
}

// WorkflowTemplateRepository is the DAO for workflow_templates.
type WorkflowTemplateRepository struct {
	db *sql.DB
}

func NewWorkflowTemplateRepository(db *sql.DB) *WorkflowTemplateRepository {
	return &WorkflowTemplateRepository{db: db}
}

const workflowTemplateColumns = `id, provider, kind, profile, name, description, content, is_builtin, created_at::text, updated_at::text`

func scanWorkflowTemplate(scanner interface{ Scan(dest ...any) error }) (*WorkflowTemplate, error) {
	var t WorkflowTemplate
	if err := scanner.Scan(&t.ID, &t.Provider, &t.Kind, &t.Profile, &t.Name, &t.Description, &t.Content, &t.IsBuiltin, &t.CreatedAt, &t.UpdatedAt); err != nil {
		return nil, err
	}
	return &t, nil
}

// GetByKey returns the template for (provider, kind, profile), or (nil, nil)
// when none exists — the caller then falls back to the embedded built-in.
func (r *WorkflowTemplateRepository) GetByKey(ctx context.Context, provider, kind, profile string) (*WorkflowTemplate, error) {
	row := r.db.QueryRowContext(ctx,
		`SELECT `+workflowTemplateColumns+` FROM workflow_templates WHERE provider = $1 AND kind = $2 AND profile = $3`,
		provider, kind, profile)
	t, err := scanWorkflowTemplate(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return t, nil
}

func (r *WorkflowTemplateRepository) GetByID(ctx context.Context, id string) (*WorkflowTemplate, error) {
	row := r.db.QueryRowContext(ctx, `SELECT `+workflowTemplateColumns+` FROM workflow_templates WHERE id = $1`, id)
	t, err := scanWorkflowTemplate(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return t, nil
}

func (r *WorkflowTemplateRepository) List(ctx context.Context) ([]WorkflowTemplate, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT `+workflowTemplateColumns+` FROM workflow_templates ORDER BY provider, kind, profile`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []WorkflowTemplate{}
	for rows.Next() {
		t, err := scanWorkflowTemplate(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

func (r *WorkflowTemplateRepository) Create(ctx context.Context, t *WorkflowTemplate) (*WorkflowTemplate, error) {
	row := r.db.QueryRowContext(ctx, `
		INSERT INTO workflow_templates (provider, kind, profile, name, description, content, is_builtin)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING `+workflowTemplateColumns,
		t.Provider, t.Kind, t.Profile, t.Name, t.Description, t.Content, t.IsBuiltin)
	return scanWorkflowTemplate(row)
}

// Update replaces the editable fields (name, description, content) of a template
// by id and bumps updated_at. Returns (nil, nil) when no row matches.
func (r *WorkflowTemplateRepository) Update(ctx context.Context, t *WorkflowTemplate) (*WorkflowTemplate, error) {
	row := r.db.QueryRowContext(ctx, `
		UPDATE workflow_templates SET name = $2, description = $3, content = $4, updated_at = now()
		WHERE id = $1
		RETURNING `+workflowTemplateColumns,
		t.ID, t.Name, t.Description, t.Content)
	updated, err := scanWorkflowTemplate(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return updated, nil
}

func (r *WorkflowTemplateRepository) Delete(ctx context.Context, id string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM workflow_templates WHERE id = $1`, id)
	return err
}

// EnsureBuiltin inserts a built-in template if its (provider, kind, profile) is
// absent, and is a no-op otherwise — safe to call on every startup.
func (r *WorkflowTemplateRepository) EnsureBuiltin(ctx context.Context, t *WorkflowTemplate) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO workflow_templates (provider, kind, profile, name, description, content, is_builtin)
		VALUES ($1, $2, $3, $4, $5, $6, true)
		ON CONFLICT (provider, kind, profile) DO NOTHING`,
		t.Provider, t.Kind, t.Profile, t.Name, t.Description, t.Content)
	return err
}
