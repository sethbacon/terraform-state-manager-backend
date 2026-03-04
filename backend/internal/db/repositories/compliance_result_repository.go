package repositories

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/terraform-state-manager/terraform-state-manager/internal/db/models"
)

// ComplianceResultRepository handles database operations for compliance results.
type ComplianceResultRepository struct {
	db *sql.DB
}

// NewComplianceResultRepository creates a new ComplianceResultRepository.
func NewComplianceResultRepository(db *sql.DB) *ComplianceResultRepository {
	return &ComplianceResultRepository{db: db}
}

// Create inserts a new compliance result into the database.
func (r *ComplianceResultRepository) Create(ctx context.Context, result *models.ComplianceResult) error {
	err := r.db.QueryRowContext(ctx,
		`INSERT INTO compliance_results (policy_id, run_id, workspace_name, status, violations)
		 VALUES ($1, $2, $3, $4, $5)
		 RETURNING id, created_at`,
		result.PolicyID,
		result.RunID,
		result.WorkspaceName,
		result.Status,
		result.Violations,
	).Scan(&result.ID, &result.CreatedAt)
	if err != nil {
		return fmt.Errorf("failed to create compliance result: %w", err)
	}
	return nil
}

// GetByID retrieves a compliance result by its ID.
func (r *ComplianceResultRepository) GetByID(ctx context.Context, id string) (*models.ComplianceResult, error) {
	var cr models.ComplianceResult
	err := r.db.QueryRowContext(ctx,
		`SELECT id, policy_id, run_id, workspace_name, status, violations, created_at
		 FROM compliance_results
		 WHERE id = $1`,
		id,
	).Scan(
		&cr.ID,
		&cr.PolicyID,
		&cr.RunID,
		&cr.WorkspaceName,
		&cr.Status,
		&cr.Violations,
		&cr.CreatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get compliance result by ID: %w", err)
	}
	return &cr, nil
}

// ListByRun returns all compliance results for a given analysis run.
func (r *ComplianceResultRepository) ListByRun(ctx context.Context, runID string, limit, offset int) ([]models.ComplianceResult, int, error) {
	var total int
	err := r.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM compliance_results WHERE run_id = $1",
		runID,
	).Scan(&total)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to count compliance results: %w", err)
	}

	rows, err := r.db.QueryContext(ctx,
		`SELECT id, policy_id, run_id, workspace_name, status, violations, created_at
		 FROM compliance_results
		 WHERE run_id = $1
		 ORDER BY created_at DESC
		 LIMIT $2 OFFSET $3`,
		runID, limit, offset,
	)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to list compliance results by run: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var results []models.ComplianceResult
	for rows.Next() {
		var cr models.ComplianceResult
		if err := rows.Scan(
			&cr.ID,
			&cr.PolicyID,
			&cr.RunID,
			&cr.WorkspaceName,
			&cr.Status,
			&cr.Violations,
			&cr.CreatedAt,
		); err != nil {
			return nil, 0, fmt.Errorf("failed to scan compliance result: %w", err)
		}
		results = append(results, cr)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("failed to iterate compliance results: %w", err)
	}
	return results, total, nil
}

// ListByPolicy returns all compliance results for a given policy.
func (r *ComplianceResultRepository) ListByPolicy(ctx context.Context, policyID string, limit, offset int) ([]models.ComplianceResult, int, error) {
	var total int
	err := r.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM compliance_results WHERE policy_id = $1",
		policyID,
	).Scan(&total)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to count compliance results by policy: %w", err)
	}

	rows, err := r.db.QueryContext(ctx,
		`SELECT id, policy_id, run_id, workspace_name, status, violations, created_at
		 FROM compliance_results
		 WHERE policy_id = $1
		 ORDER BY created_at DESC
		 LIMIT $2 OFFSET $3`,
		policyID, limit, offset,
	)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to list compliance results by policy: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var results []models.ComplianceResult
	for rows.Next() {
		var cr models.ComplianceResult
		if err := rows.Scan(
			&cr.ID,
			&cr.PolicyID,
			&cr.RunID,
			&cr.WorkspaceName,
			&cr.Status,
			&cr.Violations,
			&cr.CreatedAt,
		); err != nil {
			return nil, 0, fmt.Errorf("failed to scan compliance result: %w", err)
		}
		results = append(results, cr)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("failed to iterate compliance results by policy: %w", err)
	}
	return results, total, nil
}

// ComplianceScore holds aggregate compliance check counts.
type ComplianceScore struct {
	TotalChecks  int     `json:"total_checks"`
	PassCount    int     `json:"pass_count"`
	FailCount    int     `json:"fail_count"`
	WarningCount int     `json:"warning_count"`
	Score        float64 `json:"score_percent"`
}

// GetComplianceScore calculates the compliance score for an organization by counting
// pass, fail, and warning results across all policies belonging to that organization.
func (r *ComplianceResultRepository) GetComplianceScore(ctx context.Context, orgID string) (*ComplianceScore, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT cr.status, COUNT(*)
		 FROM compliance_results cr
		 JOIN compliance_policies cp ON cr.policy_id = cp.id
		 WHERE cp.organization_id = $1
		 GROUP BY cr.status`,
		orgID,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to get compliance score: %w", err)
	}
	defer func() { _ = rows.Close() }()

	score := &ComplianceScore{}
	for rows.Next() {
		var status string
		var count int
		if err := rows.Scan(&status, &count); err != nil {
			return nil, fmt.Errorf("failed to scan compliance score: %w", err)
		}
		switch status {
		case models.ComplianceStatusPass:
			score.PassCount = count
		case models.ComplianceStatusFail:
			score.FailCount = count
		case models.ComplianceStatusWarning:
			score.WarningCount = count
		}
		score.TotalChecks += count
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to iterate compliance score: %w", err)
	}

	if score.TotalChecks > 0 {
		score.Score = float64(score.PassCount) / float64(score.TotalChecks) * 100
	}

	return score, nil
}
