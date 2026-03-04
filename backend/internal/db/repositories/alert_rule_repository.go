package repositories

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/terraform-state-manager/terraform-state-manager/internal/db/models"
)

// AlertRuleRepository handles database operations for alert rules.
type AlertRuleRepository struct {
	db *sql.DB
}

// NewAlertRuleRepository creates a new AlertRuleRepository.
func NewAlertRuleRepository(db *sql.DB) *AlertRuleRepository {
	return &AlertRuleRepository{db: db}
}

// Create inserts a new alert rule into the database.
func (r *AlertRuleRepository) Create(ctx context.Context, rule *models.AlertRule) error {
	err := r.db.QueryRowContext(ctx,
		`INSERT INTO alert_rules (organization_id, name, rule_type, config, severity, channel_ids, is_active)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)
		 RETURNING id, created_at, updated_at`,
		rule.OrganizationID,
		rule.Name,
		rule.RuleType,
		rule.Config,
		rule.Severity,
		rule.ChannelIDs,
		rule.IsActive,
	).Scan(&rule.ID, &rule.CreatedAt, &rule.UpdatedAt)
	if err != nil {
		return fmt.Errorf("failed to create alert rule: %w", err)
	}
	return nil
}

// GetByID retrieves an alert rule by its ID.
func (r *AlertRuleRepository) GetByID(ctx context.Context, id string) (*models.AlertRule, error) {
	var rule models.AlertRule
	err := r.db.QueryRowContext(ctx,
		`SELECT id, organization_id, name, rule_type, config, severity, channel_ids, is_active, created_at, updated_at
		 FROM alert_rules
		 WHERE id = $1`,
		id,
	).Scan(
		&rule.ID,
		&rule.OrganizationID,
		&rule.Name,
		&rule.RuleType,
		&rule.Config,
		&rule.Severity,
		&rule.ChannelIDs,
		&rule.IsActive,
		&rule.CreatedAt,
		&rule.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get alert rule by ID: %w", err)
	}
	return &rule, nil
}

// ListByOrganization returns paginated alert rules for a given organization, along with the total count.
func (r *AlertRuleRepository) ListByOrganization(ctx context.Context, orgID string, limit, offset int) ([]models.AlertRule, int, error) {
	var total int
	err := r.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM alert_rules WHERE organization_id = $1",
		orgID,
	).Scan(&total)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to count alert rules: %w", err)
	}

	rows, err := r.db.QueryContext(ctx,
		`SELECT id, organization_id, name, rule_type, config, severity, channel_ids, is_active, created_at, updated_at
		 FROM alert_rules
		 WHERE organization_id = $1
		 ORDER BY created_at DESC
		 LIMIT $2 OFFSET $3`,
		orgID, limit, offset,
	)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to list alert rules: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var rules []models.AlertRule
	for rows.Next() {
		var rule models.AlertRule
		if err := rows.Scan(
			&rule.ID,
			&rule.OrganizationID,
			&rule.Name,
			&rule.RuleType,
			&rule.Config,
			&rule.Severity,
			&rule.ChannelIDs,
			&rule.IsActive,
			&rule.CreatedAt,
			&rule.UpdatedAt,
		); err != nil {
			return nil, 0, fmt.Errorf("failed to scan alert rule: %w", err)
		}
		rules = append(rules, rule)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("failed to iterate alert rules: %w", err)
	}
	return rules, total, nil
}

// GetActiveByOrganization returns all active alert rules for a given organization.
func (r *AlertRuleRepository) GetActiveByOrganization(ctx context.Context, orgID string) ([]models.AlertRule, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, organization_id, name, rule_type, config, severity, channel_ids, is_active, created_at, updated_at
		 FROM alert_rules
		 WHERE organization_id = $1 AND is_active = true
		 ORDER BY created_at DESC`,
		orgID,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to list active alert rules: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var rules []models.AlertRule
	for rows.Next() {
		var rule models.AlertRule
		if err := rows.Scan(
			&rule.ID,
			&rule.OrganizationID,
			&rule.Name,
			&rule.RuleType,
			&rule.Config,
			&rule.Severity,
			&rule.ChannelIDs,
			&rule.IsActive,
			&rule.CreatedAt,
			&rule.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("failed to scan active alert rule: %w", err)
		}
		rules = append(rules, rule)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to iterate active alert rules: %w", err)
	}
	return rules, nil
}

// Update modifies an existing alert rule.
func (r *AlertRuleRepository) Update(ctx context.Context, rule *models.AlertRule) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE alert_rules
		 SET name = $1, rule_type = $2, config = $3, severity = $4, channel_ids = $5, is_active = $6, updated_at = $7
		 WHERE id = $8`,
		rule.Name,
		rule.RuleType,
		rule.Config,
		rule.Severity,
		rule.ChannelIDs,
		rule.IsActive,
		time.Now(),
		rule.ID,
	)
	if err != nil {
		return fmt.Errorf("failed to update alert rule: %w", err)
	}
	return nil
}

// Delete removes an alert rule by its ID.
func (r *AlertRuleRepository) Delete(ctx context.Context, id string) error {
	_, err := r.db.ExecContext(ctx, "DELETE FROM alert_rules WHERE id = $1", id)
	if err != nil {
		return fmt.Errorf("failed to delete alert rule: %w", err)
	}
	return nil
}
