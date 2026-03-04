package repositories

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/terraform-state-manager/terraform-state-manager/internal/db/models"
)

// RetentionPolicyRepository handles database operations for retention policies.
type RetentionPolicyRepository struct {
	db *sql.DB
}

// NewRetentionPolicyRepository creates a new RetentionPolicyRepository.
func NewRetentionPolicyRepository(db *sql.DB) *RetentionPolicyRepository {
	return &RetentionPolicyRepository{db: db}
}

// Create inserts a new retention policy into the database.
func (r *RetentionPolicyRepository) Create(ctx context.Context, policy *models.RetentionPolicy) error {
	err := r.db.QueryRowContext(ctx,
		`INSERT INTO retention_policies (organization_id, name, max_age_days, max_count, is_default)
		 VALUES ($1, $2, $3, $4, $5)
		 RETURNING id, created_at, updated_at`,
		policy.OrganizationID,
		policy.Name,
		policy.MaxAgeDays,
		policy.MaxCount,
		policy.IsDefault,
	).Scan(&policy.ID, &policy.CreatedAt, &policy.UpdatedAt)
	if err != nil {
		return fmt.Errorf("failed to create retention policy: %w", err)
	}
	return nil
}

// GetByID retrieves a retention policy by its ID.
func (r *RetentionPolicyRepository) GetByID(ctx context.Context, id string) (*models.RetentionPolicy, error) {
	var p models.RetentionPolicy
	err := r.db.QueryRowContext(ctx,
		`SELECT id, organization_id, name, max_age_days, max_count, is_default, created_at, updated_at
		 FROM retention_policies
		 WHERE id = $1`,
		id,
	).Scan(
		&p.ID,
		&p.OrganizationID,
		&p.Name,
		&p.MaxAgeDays,
		&p.MaxCount,
		&p.IsDefault,
		&p.CreatedAt,
		&p.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get retention policy by ID: %w", err)
	}
	return &p, nil
}

// Update modifies an existing retention policy.
func (r *RetentionPolicyRepository) Update(ctx context.Context, policy *models.RetentionPolicy) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE retention_policies
		 SET name = $1, max_age_days = $2, max_count = $3, is_default = $4, updated_at = $5
		 WHERE id = $6`,
		policy.Name,
		policy.MaxAgeDays,
		policy.MaxCount,
		policy.IsDefault,
		time.Now(),
		policy.ID,
	)
	if err != nil {
		return fmt.Errorf("failed to update retention policy: %w", err)
	}
	return nil
}

// Delete removes a retention policy by its ID.
func (r *RetentionPolicyRepository) Delete(ctx context.Context, id string) error {
	_, err := r.db.ExecContext(ctx, "DELETE FROM retention_policies WHERE id = $1", id)
	if err != nil {
		return fmt.Errorf("failed to delete retention policy: %w", err)
	}
	return nil
}

// ListByOrganization returns all retention policies for a given organization.
func (r *RetentionPolicyRepository) ListByOrganization(ctx context.Context, orgID string) ([]models.RetentionPolicy, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, organization_id, name, max_age_days, max_count, is_default, created_at, updated_at
		 FROM retention_policies
		 WHERE organization_id = $1
		 ORDER BY name`,
		orgID,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to list retention policies: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var policies []models.RetentionPolicy
	for rows.Next() {
		var p models.RetentionPolicy
		if err := rows.Scan(
			&p.ID,
			&p.OrganizationID,
			&p.Name,
			&p.MaxAgeDays,
			&p.MaxCount,
			&p.IsDefault,
			&p.CreatedAt,
			&p.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("failed to scan retention policy: %w", err)
		}
		policies = append(policies, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to iterate retention policies: %w", err)
	}
	return policies, nil
}

// GetDefault returns the default retention policy for an organization, if one exists.
func (r *RetentionPolicyRepository) GetDefault(ctx context.Context, orgID string) (*models.RetentionPolicy, error) {
	var p models.RetentionPolicy
	err := r.db.QueryRowContext(ctx,
		`SELECT id, organization_id, name, max_age_days, max_count, is_default, created_at, updated_at
		 FROM retention_policies
		 WHERE organization_id = $1 AND is_default = true
		 LIMIT 1`,
		orgID,
	).Scan(
		&p.ID,
		&p.OrganizationID,
		&p.Name,
		&p.MaxAgeDays,
		&p.MaxCount,
		&p.IsDefault,
		&p.CreatedAt,
		&p.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get default retention policy: %w", err)
	}
	return &p, nil
}
