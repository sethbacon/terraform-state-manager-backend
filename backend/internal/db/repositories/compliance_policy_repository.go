package repositories

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/terraform-state-manager/terraform-state-manager/internal/db/models"
)

// CompliancePolicyRepository handles database operations for compliance policies.
type CompliancePolicyRepository struct {
	db *sql.DB
}

// NewCompliancePolicyRepository creates a new CompliancePolicyRepository.
func NewCompliancePolicyRepository(db *sql.DB) *CompliancePolicyRepository {
	return &CompliancePolicyRepository{db: db}
}

// Create inserts a new compliance policy into the database.
func (r *CompliancePolicyRepository) Create(ctx context.Context, policy *models.CompliancePolicy) error {
	err := r.db.QueryRowContext(ctx,
		`INSERT INTO compliance_policies (organization_id, name, policy_type, config, severity, is_active)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 RETURNING id, created_at, updated_at`,
		policy.OrganizationID,
		policy.Name,
		policy.PolicyType,
		policy.Config,
		policy.Severity,
		policy.IsActive,
	).Scan(&policy.ID, &policy.CreatedAt, &policy.UpdatedAt)
	if err != nil {
		return fmt.Errorf("failed to create compliance policy: %w", err)
	}
	return nil
}

// GetByID retrieves a compliance policy by its ID.
func (r *CompliancePolicyRepository) GetByID(ctx context.Context, id string) (*models.CompliancePolicy, error) {
	var p models.CompliancePolicy
	err := r.db.QueryRowContext(ctx,
		`SELECT id, organization_id, name, policy_type, config, severity, is_active, created_at, updated_at
		 FROM compliance_policies
		 WHERE id = $1`,
		id,
	).Scan(
		&p.ID,
		&p.OrganizationID,
		&p.Name,
		&p.PolicyType,
		&p.Config,
		&p.Severity,
		&p.IsActive,
		&p.CreatedAt,
		&p.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get compliance policy by ID: %w", err)
	}
	return &p, nil
}

// ListByOrganization returns paginated compliance policies for a given organization, along with the total count.
func (r *CompliancePolicyRepository) ListByOrganization(ctx context.Context, orgID string, limit, offset int) ([]models.CompliancePolicy, int, error) {
	var total int
	err := r.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM compliance_policies WHERE organization_id = $1",
		orgID,
	).Scan(&total)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to count compliance policies: %w", err)
	}

	rows, err := r.db.QueryContext(ctx,
		`SELECT id, organization_id, name, policy_type, config, severity, is_active, created_at, updated_at
		 FROM compliance_policies
		 WHERE organization_id = $1
		 ORDER BY name
		 LIMIT $2 OFFSET $3`,
		orgID, limit, offset,
	)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to list compliance policies: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var policies []models.CompliancePolicy
	for rows.Next() {
		var p models.CompliancePolicy
		if err := rows.Scan(
			&p.ID,
			&p.OrganizationID,
			&p.Name,
			&p.PolicyType,
			&p.Config,
			&p.Severity,
			&p.IsActive,
			&p.CreatedAt,
			&p.UpdatedAt,
		); err != nil {
			return nil, 0, fmt.Errorf("failed to scan compliance policy: %w", err)
		}
		policies = append(policies, p)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("failed to iterate compliance policies: %w", err)
	}
	return policies, total, nil
}

// GetActiveByOrganization returns all active compliance policies for a given organization.
func (r *CompliancePolicyRepository) GetActiveByOrganization(ctx context.Context, orgID string) ([]models.CompliancePolicy, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, organization_id, name, policy_type, config, severity, is_active, created_at, updated_at
		 FROM compliance_policies
		 WHERE organization_id = $1 AND is_active = true
		 ORDER BY name`,
		orgID,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to list active compliance policies: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var policies []models.CompliancePolicy
	for rows.Next() {
		var p models.CompliancePolicy
		if err := rows.Scan(
			&p.ID,
			&p.OrganizationID,
			&p.Name,
			&p.PolicyType,
			&p.Config,
			&p.Severity,
			&p.IsActive,
			&p.CreatedAt,
			&p.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("failed to scan active compliance policy: %w", err)
		}
		policies = append(policies, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to iterate active compliance policies: %w", err)
	}
	return policies, nil
}

// Update modifies an existing compliance policy.
func (r *CompliancePolicyRepository) Update(ctx context.Context, policy *models.CompliancePolicy) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE compliance_policies
		 SET name = $1, policy_type = $2, config = $3, severity = $4, is_active = $5, updated_at = $6
		 WHERE id = $7`,
		policy.Name,
		policy.PolicyType,
		policy.Config,
		policy.Severity,
		policy.IsActive,
		time.Now(),
		policy.ID,
	)
	if err != nil {
		return fmt.Errorf("failed to update compliance policy: %w", err)
	}
	return nil
}

// Delete removes a compliance policy by its ID.
func (r *CompliancePolicyRepository) Delete(ctx context.Context, id string) error {
	_, err := r.db.ExecContext(ctx, "DELETE FROM compliance_policies WHERE id = $1", id)
	if err != nil {
		return fmt.Errorf("failed to delete compliance policy: %w", err)
	}
	return nil
}
