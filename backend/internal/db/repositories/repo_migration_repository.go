package repositories

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/terraform-state-manager/terraform-state-manager/internal/db/models"
)

// RepoMigrationRepository persists Azure DevOps repo-migration EXECUTE runs and
// their per-resource checkpoint steps. It is the production implementation of
// the orchestrator's CheckpointStore (see services/repomigration).
type RepoMigrationRepository struct {
	db *sql.DB
}

// NewRepoMigrationRepository creates a new RepoMigrationRepository.
func NewRepoMigrationRepository(db *sql.DB) *RepoMigrationRepository {
	return &RepoMigrationRepository{db: db}
}

// CreateMigration inserts a new repo-migration run and populates its generated
// id and timestamps.
func (r *RepoMigrationRepository) CreateMigration(ctx context.Context, m *models.RepoMigration) error {
	err := r.db.QueryRowContext(ctx,
		`INSERT INTO repo_migrations (
			organization_id, source_org_url, source_project,
			target_org_url, target_project, status, created_by
		) VALUES ($1, $2, $3, $4, $5, $6, $7)
		 RETURNING id, created_at, updated_at`,
		m.OrganizationID,
		m.SourceOrgURL,
		m.SourceProject,
		m.TargetOrgURL,
		m.TargetProject,
		m.Status,
		m.CreatedBy,
	).Scan(&m.ID, &m.CreatedAt, &m.UpdatedAt)
	if err != nil {
		return fmt.Errorf("failed to create repo migration: %w", err)
	}
	return nil
}

// GetMigration retrieves a repo-migration run by id, or (nil, nil) if absent.
func (r *RepoMigrationRepository) GetMigration(ctx context.Context, id string) (*models.RepoMigration, error) {
	var m models.RepoMigration
	err := r.db.QueryRowContext(ctx,
		`SELECT id, organization_id, source_org_url, source_project,
		        target_org_url, target_project, status, total_resources,
		        created_resources, skipped_resources, failed_resources,
		        started_at, completed_at, created_by, created_at, updated_at
		 FROM repo_migrations WHERE id = $1`,
		id,
	).Scan(
		&m.ID, &m.OrganizationID, &m.SourceOrgURL, &m.SourceProject,
		&m.TargetOrgURL, &m.TargetProject, &m.Status, &m.TotalResources,
		&m.CreatedResources, &m.SkippedResources, &m.FailedResources,
		&m.StartedAt, &m.CompletedAt, &m.CreatedBy, &m.CreatedAt, &m.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get repo migration: %w", err)
	}
	return &m, nil
}

// UpdateMigration persists status, counters, and completion timestamps for a run.
func (r *RepoMigrationRepository) UpdateMigration(ctx context.Context, m *models.RepoMigration) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE repo_migrations
		 SET status = $1, total_resources = $2, created_resources = $3,
		     skipped_resources = $4, failed_resources = $5,
		     started_at = $6, completed_at = $7, updated_at = $8
		 WHERE id = $9`,
		m.Status,
		m.TotalResources,
		m.CreatedResources,
		m.SkippedResources,
		m.FailedResources,
		m.StartedAt,
		m.CompletedAt,
		time.Now(),
		m.ID,
	)
	if err != nil {
		return fmt.Errorf("failed to update repo migration: %w", err)
	}
	return nil
}

// ListSteps returns all checkpoint steps for a run, oldest first.
func (r *RepoMigrationRepository) ListSteps(ctx context.Context, migrationID string) ([]models.RepoMigrationStep, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, migration_id, resource_type, resource_key, status,
		        detail, error, created_at, updated_at
		 FROM repo_migration_steps
		 WHERE migration_id = $1
		 ORDER BY created_at ASC`,
		migrationID,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to list repo migration steps: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var steps []models.RepoMigrationStep
	for rows.Next() {
		var s models.RepoMigrationStep
		if err := rows.Scan(
			&s.ID, &s.MigrationID, &s.ResourceType, &s.ResourceKey, &s.Status,
			&s.Detail, &s.Error, &s.CreatedAt, &s.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("failed to scan repo migration step: %w", err)
		}
		steps = append(steps, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to iterate repo migration steps: %w", err)
	}
	return steps, nil
}

// UpsertStep records a checkpoint for one resource. It is keyed on the unique
// (migration_id, resource_type, resource_key) triple: a conflict updates the
// existing row's status/detail/error. This is what makes a run idempotent and
// resumable — a re-recorded step overwrites rather than duplicates.
func (r *RepoMigrationRepository) UpsertStep(ctx context.Context, s *models.RepoMigrationStep) error {
	err := r.db.QueryRowContext(ctx,
		`INSERT INTO repo_migration_steps (
			migration_id, resource_type, resource_key, status, detail, error
		) VALUES ($1, $2, $3, $4, $5, $6)
		 ON CONFLICT (migration_id, resource_type, resource_key)
		 DO UPDATE SET status = EXCLUDED.status,
		               detail = EXCLUDED.detail,
		               error  = EXCLUDED.error,
		               updated_at = NOW()
		 RETURNING id, created_at, updated_at`,
		s.MigrationID,
		s.ResourceType,
		s.ResourceKey,
		s.Status,
		s.Detail,
		s.Error,
	).Scan(&s.ID, &s.CreatedAt, &s.UpdatedAt)
	if err != nil {
		return fmt.Errorf("failed to upsert repo migration step: %w", err)
	}
	return nil
}
