package repositories

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/terraform-state-manager/terraform-state-manager/internal/db/models"
)

// MigrationRepository handles database operations for migration jobs.
type MigrationRepository struct {
	db *sql.DB
}

// NewMigrationRepository creates a new MigrationRepository.
func NewMigrationRepository(db *sql.DB) *MigrationRepository {
	return &MigrationRepository{db: db}
}

// Create inserts a new migration job into the database.
func (r *MigrationRepository) Create(ctx context.Context, job *models.MigrationJob) error {
	err := r.db.QueryRowContext(ctx,
		`INSERT INTO migration_jobs (
			organization_id, name, source_backend, source_config,
			target_backend, target_config, status, dry_run, created_by
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		 RETURNING id, created_at, updated_at`,
		job.OrganizationID,
		job.Name,
		job.SourceBackend,
		job.SourceConfig,
		job.TargetBackend,
		job.TargetConfig,
		job.Status,
		job.DryRun,
		job.CreatedBy,
	).Scan(&job.ID, &job.CreatedAt, &job.UpdatedAt)
	if err != nil {
		return fmt.Errorf("failed to create migration job: %w", err)
	}
	return nil
}

// GetByID retrieves a migration job by its ID.
func (r *MigrationRepository) GetByID(ctx context.Context, id string) (*models.MigrationJob, error) {
	var j models.MigrationJob
	err := r.db.QueryRowContext(ctx,
		`SELECT id, organization_id, name, source_backend, source_config,
		        target_backend, target_config, status, total_files,
		        migrated_files, failed_files, skipped_files, error_log,
		        dry_run, started_at, completed_at, created_by, created_at, updated_at
		 FROM migration_jobs
		 WHERE id = $1`,
		id,
	).Scan(
		&j.ID,
		&j.OrganizationID,
		&j.Name,
		&j.SourceBackend,
		&j.SourceConfig,
		&j.TargetBackend,
		&j.TargetConfig,
		&j.Status,
		&j.TotalFiles,
		&j.MigratedFiles,
		&j.FailedFiles,
		&j.SkippedFiles,
		&j.ErrorLog,
		&j.DryRun,
		&j.StartedAt,
		&j.CompletedAt,
		&j.CreatedBy,
		&j.CreatedAt,
		&j.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get migration job by ID: %w", err)
	}
	return &j, nil
}

// ListByOrganization returns paginated migration jobs for a given organization, along with the total count.
func (r *MigrationRepository) ListByOrganization(ctx context.Context, orgID string, limit, offset int) ([]models.MigrationJob, int, error) {
	var total int
	err := r.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM migration_jobs WHERE organization_id = $1",
		orgID,
	).Scan(&total)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to count migration jobs: %w", err)
	}

	rows, err := r.db.QueryContext(ctx,
		`SELECT id, organization_id, name, source_backend, source_config,
		        target_backend, target_config, status, total_files,
		        migrated_files, failed_files, skipped_files, error_log,
		        dry_run, started_at, completed_at, created_by, created_at, updated_at
		 FROM migration_jobs
		 WHERE organization_id = $1
		 ORDER BY created_at DESC
		 LIMIT $2 OFFSET $3`,
		orgID, limit, offset,
	)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to list migration jobs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var jobs []models.MigrationJob
	for rows.Next() {
		var j models.MigrationJob
		if err := rows.Scan(
			&j.ID,
			&j.OrganizationID,
			&j.Name,
			&j.SourceBackend,
			&j.SourceConfig,
			&j.TargetBackend,
			&j.TargetConfig,
			&j.Status,
			&j.TotalFiles,
			&j.MigratedFiles,
			&j.FailedFiles,
			&j.SkippedFiles,
			&j.ErrorLog,
			&j.DryRun,
			&j.StartedAt,
			&j.CompletedAt,
			&j.CreatedBy,
			&j.CreatedAt,
			&j.UpdatedAt,
		); err != nil {
			return nil, 0, fmt.Errorf("failed to scan migration job: %w", err)
		}
		jobs = append(jobs, j)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("failed to iterate migration jobs: %w", err)
	}
	return jobs, total, nil
}

// Update modifies an existing migration job.
func (r *MigrationRepository) Update(ctx context.Context, job *models.MigrationJob) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE migration_jobs
		 SET name = $1, status = $2, total_files = $3, migrated_files = $4,
		     failed_files = $5, skipped_files = $6, error_log = $7,
		     started_at = $8, completed_at = $9, updated_at = $10
		 WHERE id = $11`,
		job.Name,
		job.Status,
		job.TotalFiles,
		job.MigratedFiles,
		job.FailedFiles,
		job.SkippedFiles,
		job.ErrorLog,
		job.StartedAt,
		job.CompletedAt,
		time.Now(),
		job.ID,
	)
	if err != nil {
		return fmt.Errorf("failed to update migration job: %w", err)
	}
	return nil
}

// UpdateProgress updates the file counters for a running migration job.
func (r *MigrationRepository) UpdateProgress(ctx context.Context, id string, migratedFiles, failedFiles int) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE migration_jobs
		 SET migrated_files = $1, failed_files = $2, updated_at = $3
		 WHERE id = $4`,
		migratedFiles,
		failedFiles,
		time.Now(),
		id,
	)
	if err != nil {
		return fmt.Errorf("failed to update migration progress: %w", err)
	}
	return nil
}
