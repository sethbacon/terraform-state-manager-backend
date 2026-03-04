package repositories

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/terraform-state-manager/terraform-state-manager/internal/db/models"
)

// AnalysisSummary holds aggregated metrics from the latest completed analysis run.
type AnalysisSummary struct {
	TotalRUM         int `json:"total_rum"`
	TotalManaged     int `json:"total_managed"`
	TotalResources   int `json:"total_resources"`
	TotalDataSources int `json:"total_data_sources"`
	TotalWorkspaces  int `json:"total_workspaces"`
}

// AnalysisResultRepository handles database operations for analysis results.
type AnalysisResultRepository struct {
	db *sql.DB
}

// NewAnalysisResultRepository creates a new AnalysisResultRepository.
func NewAnalysisResultRepository(db *sql.DB) *AnalysisResultRepository {
	return &AnalysisResultRepository{db: db}
}

// Create inserts a new analysis result into the database.
func (r *AnalysisResultRepository) Create(ctx context.Context, result *models.AnalysisResult) error {
	err := r.db.QueryRowContext(ctx,
		`INSERT INTO analysis_results (
			run_id, workspace_id, workspace_name, organization, status,
			error_type, error_message, total_resources, managed_count, rum_count,
			data_source_count, null_resource_count, resources_by_type, resources_by_module,
			provider_analysis, terraform_version, state_serial, state_lineage,
			last_modified, analysis_method, raw_state_hash
		) VALUES (
			$1, $2, $3, $4, $5,
			$6, $7, $8, $9, $10,
			$11, $12, $13, $14,
			$15, $16, $17, $18,
			$19, $20, $21
		) RETURNING id, created_at`,
		result.RunID,
		result.WorkspaceID,
		result.WorkspaceName,
		result.Organization,
		result.Status,
		result.ErrorType,
		result.ErrorMessage,
		result.TotalResources,
		result.ManagedCount,
		result.RUMCount,
		result.DataSourceCount,
		result.NullResourceCount,
		result.ResourcesByType,
		result.ResourcesByModule,
		result.ProviderAnalysis,
		result.TerraformVersion,
		result.StateSerial,
		result.StateLineage,
		result.LastModified,
		result.AnalysisMethod,
		result.RawStateHash,
	).Scan(&result.ID, &result.CreatedAt)
	if err != nil {
		return fmt.Errorf("failed to create analysis result: %w", err)
	}
	return nil
}

// BulkCreate inserts multiple analysis results in a single query for performance.
func (r *AnalysisResultRepository) BulkCreate(ctx context.Context, results []models.AnalysisResult) error {
	if len(results) == 0 {
		return nil
	}

	const colCount = 21
	valueStrings := make([]string, 0, len(results))
	valueArgs := make([]interface{}, 0, len(results)*colCount)

	for i, res := range results {
		base := i * colCount
		valueStrings = append(valueStrings, fmt.Sprintf(
			"($%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d)",
			base+1, base+2, base+3, base+4, base+5,
			base+6, base+7, base+8, base+9, base+10,
			base+11, base+12, base+13, base+14,
			base+15, base+16, base+17, base+18,
			base+19, base+20, base+21,
		))
		valueArgs = append(valueArgs,
			res.RunID,
			res.WorkspaceID,
			res.WorkspaceName,
			res.Organization,
			res.Status,
			res.ErrorType,
			res.ErrorMessage,
			res.TotalResources,
			res.ManagedCount,
			res.RUMCount,
			res.DataSourceCount,
			res.NullResourceCount,
			res.ResourcesByType,
			res.ResourcesByModule,
			res.ProviderAnalysis,
			res.TerraformVersion,
			res.StateSerial,
			res.StateLineage,
			res.LastModified,
			res.AnalysisMethod,
			res.RawStateHash,
		)
	}

	query := fmt.Sprintf(
		`INSERT INTO analysis_results (
			run_id, workspace_id, workspace_name, organization, status,
			error_type, error_message, total_resources, managed_count, rum_count,
			data_source_count, null_resource_count, resources_by_type, resources_by_module,
			provider_analysis, terraform_version, state_serial, state_lineage,
			last_modified, analysis_method, raw_state_hash
		) VALUES %s`,
		strings.Join(valueStrings, ", "),
	)

	_, err := r.db.ExecContext(ctx, query, valueArgs...)
	if err != nil {
		return fmt.Errorf("failed to bulk create analysis results: %w", err)
	}
	return nil
}

// GetByID retrieves an analysis result by its ID.
func (r *AnalysisResultRepository) GetByID(ctx context.Context, id string) (*models.AnalysisResult, error) {
	var res models.AnalysisResult
	err := r.db.QueryRowContext(ctx,
		`SELECT id, run_id, workspace_id, workspace_name, organization, status,
		        error_type, error_message, total_resources, managed_count, rum_count,
		        data_source_count, null_resource_count, resources_by_type, resources_by_module,
		        provider_analysis, terraform_version, state_serial, state_lineage,
		        last_modified, analysis_method, raw_state_hash, created_at
		 FROM analysis_results
		 WHERE id = $1`,
		id,
	).Scan(
		&res.ID,
		&res.RunID,
		&res.WorkspaceID,
		&res.WorkspaceName,
		&res.Organization,
		&res.Status,
		&res.ErrorType,
		&res.ErrorMessage,
		&res.TotalResources,
		&res.ManagedCount,
		&res.RUMCount,
		&res.DataSourceCount,
		&res.NullResourceCount,
		&res.ResourcesByType,
		&res.ResourcesByModule,
		&res.ProviderAnalysis,
		&res.TerraformVersion,
		&res.StateSerial,
		&res.StateLineage,
		&res.LastModified,
		&res.AnalysisMethod,
		&res.RawStateHash,
		&res.CreatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get analysis result by ID: %w", err)
	}
	return &res, nil
}

// ListByRunID returns paginated analysis results for a given run, along with the total count.
func (r *AnalysisResultRepository) ListByRunID(ctx context.Context, runID string, limit, offset int) ([]models.AnalysisResult, int, error) {
	var total int
	err := r.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM analysis_results WHERE run_id = $1",
		runID,
	).Scan(&total)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to count analysis results: %w", err)
	}

	rows, err := r.db.QueryContext(ctx,
		`SELECT id, run_id, workspace_id, workspace_name, organization, status,
		        error_type, error_message, total_resources, managed_count, rum_count,
		        data_source_count, null_resource_count, resources_by_type, resources_by_module,
		        provider_analysis, terraform_version, state_serial, state_lineage,
		        last_modified, analysis_method, raw_state_hash, created_at
		 FROM analysis_results
		 WHERE run_id = $1
		 ORDER BY workspace_name
		 LIMIT $2 OFFSET $3`,
		runID, limit, offset,
	)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to list analysis results: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var results []models.AnalysisResult
	for rows.Next() {
		var res models.AnalysisResult
		if err := rows.Scan(
			&res.ID,
			&res.RunID,
			&res.WorkspaceID,
			&res.WorkspaceName,
			&res.Organization,
			&res.Status,
			&res.ErrorType,
			&res.ErrorMessage,
			&res.TotalResources,
			&res.ManagedCount,
			&res.RUMCount,
			&res.DataSourceCount,
			&res.NullResourceCount,
			&res.ResourcesByType,
			&res.ResourcesByModule,
			&res.ProviderAnalysis,
			&res.TerraformVersion,
			&res.StateSerial,
			&res.StateLineage,
			&res.LastModified,
			&res.AnalysisMethod,
			&res.RawStateHash,
			&res.CreatedAt,
		); err != nil {
			return nil, 0, fmt.Errorf("failed to scan analysis result: %w", err)
		}
		results = append(results, res)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("failed to iterate analysis results: %w", err)
	}
	return results, total, nil
}

// GetLatestSummary returns aggregated metrics from the most recent completed analysis run
// for a given organization.
func (r *AnalysisResultRepository) GetLatestSummary(ctx context.Context, orgID string) (*AnalysisSummary, error) {
	var summary AnalysisSummary
	err := r.db.QueryRowContext(ctx,
		`SELECT
			COALESCE(ar.total_rum, 0),
			COALESCE(ar.total_managed, 0),
			COALESCE(ar.total_resources, 0),
			COALESCE(ar.total_data_sources, 0),
			COALESCE(ar.total_workspaces, 0)
		 FROM analysis_runs ar
		 WHERE ar.organization_id = $1 AND ar.status = $2
		 ORDER BY ar.completed_at DESC
		 LIMIT 1`,
		orgID, models.RunStatusCompleted,
	).Scan(
		&summary.TotalRUM,
		&summary.TotalManaged,
		&summary.TotalResources,
		&summary.TotalDataSources,
		&summary.TotalWorkspaces,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get latest analysis summary: %w", err)
	}
	return &summary, nil
}

// GetAllByRunID returns all analysis results for a given run without pagination.
func (r *AnalysisResultRepository) GetAllByRunID(ctx context.Context, runID string) ([]*models.AnalysisResult, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, run_id, workspace_id, workspace_name, organization, status,
		        error_type, error_message, total_resources, managed_count, rum_count,
		        data_source_count, null_resource_count, resources_by_type, resources_by_module,
		        provider_analysis, terraform_version, state_serial, state_lineage,
		        last_modified, analysis_method, raw_state_hash, created_at
		 FROM analysis_results
		 WHERE run_id = $1
		 ORDER BY workspace_name`,
		runID,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to list all analysis results by run: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var results []*models.AnalysisResult
	for rows.Next() {
		var res models.AnalysisResult
		if err := rows.Scan(
			&res.ID,
			&res.RunID,
			&res.WorkspaceID,
			&res.WorkspaceName,
			&res.Organization,
			&res.Status,
			&res.ErrorType,
			&res.ErrorMessage,
			&res.TotalResources,
			&res.ManagedCount,
			&res.RUMCount,
			&res.DataSourceCount,
			&res.NullResourceCount,
			&res.ResourcesByType,
			&res.ResourcesByModule,
			&res.ProviderAnalysis,
			&res.TerraformVersion,
			&res.StateSerial,
			&res.StateLineage,
			&res.LastModified,
			&res.AnalysisMethod,
			&res.RawStateHash,
			&res.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("failed to scan analysis result: %w", err)
		}
		results = append(results, &res)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to iterate analysis results: %w", err)
	}
	return results, nil
}
