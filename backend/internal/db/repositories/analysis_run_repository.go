package repositories

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/terraform-state-manager/terraform-state-manager/internal/db/models"
)

// AnalysisRunRepository handles database operations for analysis runs.
type AnalysisRunRepository struct {
	db *sql.DB
}

// NewAnalysisRunRepository creates a new AnalysisRunRepository.
func NewAnalysisRunRepository(db *sql.DB) *AnalysisRunRepository {
	return &AnalysisRunRepository{db: db}
}

// Create inserts a new analysis run into the database.
func (r *AnalysisRunRepository) Create(ctx context.Context, run *models.AnalysisRun) error {
	err := r.db.QueryRowContext(ctx,
		`INSERT INTO analysis_runs (organization_id, source_id, status, trigger_type, config, triggered_by)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 RETURNING id, created_at, updated_at`,
		run.OrganizationID,
		run.SourceID,
		run.Status,
		run.TriggerType,
		run.Config,
		run.TriggeredBy,
	).Scan(&run.ID, &run.CreatedAt, &run.UpdatedAt)
	if err != nil {
		return fmt.Errorf("failed to create analysis run: %w", err)
	}
	return nil
}

// GetByID retrieves an analysis run by its ID.
func (r *AnalysisRunRepository) GetByID(ctx context.Context, id string) (*models.AnalysisRun, error) {
	var run models.AnalysisRun
	err := r.db.QueryRowContext(ctx,
		`SELECT id, organization_id, source_id, status, trigger_type, config,
		        started_at, completed_at, total_workspaces, successful_count, failed_count,
		        total_rum, total_managed, total_resources, total_data_sources,
		        error_message, performance_ms, triggered_by, created_at, updated_at
		 FROM analysis_runs
		 WHERE id = $1`,
		id,
	).Scan(
		&run.ID,
		&run.OrganizationID,
		&run.SourceID,
		&run.Status,
		&run.TriggerType,
		&run.Config,
		&run.StartedAt,
		&run.CompletedAt,
		&run.TotalWorkspaces,
		&run.SuccessfulCount,
		&run.FailedCount,
		&run.TotalRUM,
		&run.TotalManaged,
		&run.TotalResources,
		&run.TotalDataSources,
		&run.ErrorMessage,
		&run.PerformanceMS,
		&run.TriggeredBy,
		&run.CreatedAt,
		&run.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get analysis run by ID: %w", err)
	}
	return &run, nil
}

// Update modifies an existing analysis run.
func (r *AnalysisRunRepository) Update(ctx context.Context, run *models.AnalysisRun) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE analysis_runs
		 SET status = $1, started_at = $2, completed_at = $3,
		     total_workspaces = $4, successful_count = $5, failed_count = $6,
		     total_rum = $7, total_managed = $8, total_resources = $9, total_data_sources = $10,
		     error_message = $11, performance_ms = $12, updated_at = $13
		 WHERE id = $14`,
		run.Status,
		run.StartedAt,
		run.CompletedAt,
		run.TotalWorkspaces,
		run.SuccessfulCount,
		run.FailedCount,
		run.TotalRUM,
		run.TotalManaged,
		run.TotalResources,
		run.TotalDataSources,
		run.ErrorMessage,
		run.PerformanceMS,
		time.Now(),
		run.ID,
	)
	if err != nil {
		return fmt.Errorf("failed to update analysis run: %w", err)
	}
	return nil
}

// ListByOrganization returns paginated analysis runs for a given organization, along with the total count.
func (r *AnalysisRunRepository) ListByOrganization(ctx context.Context, orgID string, limit, offset int) ([]models.AnalysisRun, int, error) {
	var total int
	err := r.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM analysis_runs WHERE organization_id = $1",
		orgID,
	).Scan(&total)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to count analysis runs: %w", err)
	}

	rows, err := r.db.QueryContext(ctx,
		`SELECT id, organization_id, source_id, status, trigger_type, config,
		        started_at, completed_at, total_workspaces, successful_count, failed_count,
		        total_rum, total_managed, total_resources, total_data_sources,
		        error_message, performance_ms, triggered_by, created_at, updated_at
		 FROM analysis_runs
		 WHERE organization_id = $1
		 ORDER BY created_at DESC
		 LIMIT $2 OFFSET $3`,
		orgID, limit, offset,
	)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to list analysis runs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var runs []models.AnalysisRun
	for rows.Next() {
		var run models.AnalysisRun
		if err := rows.Scan(
			&run.ID,
			&run.OrganizationID,
			&run.SourceID,
			&run.Status,
			&run.TriggerType,
			&run.Config,
			&run.StartedAt,
			&run.CompletedAt,
			&run.TotalWorkspaces,
			&run.SuccessfulCount,
			&run.FailedCount,
			&run.TotalRUM,
			&run.TotalManaged,
			&run.TotalResources,
			&run.TotalDataSources,
			&run.ErrorMessage,
			&run.PerformanceMS,
			&run.TriggeredBy,
			&run.CreatedAt,
			&run.UpdatedAt,
		); err != nil {
			return nil, 0, fmt.Errorf("failed to scan analysis run: %w", err)
		}
		runs = append(runs, run)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("failed to iterate analysis runs: %w", err)
	}
	return runs, total, nil
}

// UpdateStatus sets the status of an analysis run and updates the timestamp.
func (r *AnalysisRunRepository) UpdateStatus(ctx context.Context, id string, status string) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE analysis_runs SET status = $1, updated_at = $2 WHERE id = $3`,
		status, time.Now(), id,
	)
	if err != nil {
		return fmt.Errorf("failed to update analysis run status: %w", err)
	}
	return nil
}

// UpdateSummary sets the aggregate counters and performance metric for a completed analysis run.
func (r *AnalysisRunRepository) UpdateSummary(ctx context.Context, id string, totalWorkspaces, successCount, failCount, totalRUM, totalManaged, totalResources, totalDataSources, performanceMS int) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE analysis_runs
		 SET total_workspaces = $1, successful_count = $2, failed_count = $3,
		     total_rum = $4, total_managed = $5, total_resources = $6, total_data_sources = $7,
		     performance_ms = $8, updated_at = $9
		 WHERE id = $10`,
		totalWorkspaces,
		successCount,
		failCount,
		totalRUM,
		totalManaged,
		totalResources,
		totalDataSources,
		performanceMS,
		time.Now(),
		id,
	)
	if err != nil {
		return fmt.Errorf("failed to update analysis run summary: %w", err)
	}
	return nil
}

// GetLatestCompletedBySource retrieves the most recent completed analysis run for
// a given organization and source.
func (r *AnalysisRunRepository) GetLatestCompletedBySource(ctx context.Context, orgID, sourceID string) (*models.AnalysisRun, error) {
	var run models.AnalysisRun
	err := r.db.QueryRowContext(ctx,
		`SELECT id, organization_id, source_id, status, trigger_type, config,
		        started_at, completed_at, total_workspaces, successful_count, failed_count,
		        total_rum, total_managed, total_resources, total_data_sources,
		        error_message, performance_ms, triggered_by, created_at, updated_at
		 FROM analysis_runs
		 WHERE organization_id = $1 AND source_id = $2 AND status = $3
		 ORDER BY completed_at DESC
		 LIMIT 1`,
		orgID, sourceID, models.RunStatusCompleted,
	).Scan(
		&run.ID,
		&run.OrganizationID,
		&run.SourceID,
		&run.Status,
		&run.TriggerType,
		&run.Config,
		&run.StartedAt,
		&run.CompletedAt,
		&run.TotalWorkspaces,
		&run.SuccessfulCount,
		&run.FailedCount,
		&run.TotalRUM,
		&run.TotalManaged,
		&run.TotalResources,
		&run.TotalDataSources,
		&run.ErrorMessage,
		&run.PerformanceMS,
		&run.TriggeredBy,
		&run.CreatedAt,
		&run.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get latest completed analysis run by source: %w", err)
	}
	return &run, nil
}

// Delete removes an analysis run by ID. Associated results are cascade-deleted by the database.
func (r *AnalysisRunRepository) Delete(ctx context.Context, id string) error {
	result, err := r.db.ExecContext(ctx, `DELETE FROM analysis_runs WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("failed to delete analysis run: %w", err)
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		return fmt.Errorf("analysis run not found")
	}
	return nil
}

