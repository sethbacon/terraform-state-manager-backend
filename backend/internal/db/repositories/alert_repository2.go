package repositories

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/terraform-state-manager/terraform-state-manager/internal/db/models"
)

// AlertsRepository handles database operations for alerts.
// Named AlertsRepository to avoid conflict with the existing AuditRepository-based alert handling.
type AlertsRepository struct {
	db *sql.DB
}

// NewAlertsRepository creates a new AlertsRepository.
func NewAlertsRepository(db *sql.DB) *AlertsRepository {
	return &AlertsRepository{db: db}
}

// Create inserts a new alert into the database.
func (r *AlertsRepository) Create(ctx context.Context, alert *models.Alert) error {
	err := r.db.QueryRowContext(ctx,
		`INSERT INTO alerts (organization_id, rule_id, workspace_name, severity, title, description, metadata)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)
		 RETURNING id, created_at`,
		alert.OrganizationID,
		alert.RuleID,
		alert.WorkspaceName,
		alert.Severity,
		alert.Title,
		alert.Description,
		alert.Metadata,
	).Scan(&alert.ID, &alert.CreatedAt)
	if err != nil {
		return fmt.Errorf("failed to create alert: %w", err)
	}
	return nil
}

// GetByID retrieves an alert by its ID.
func (r *AlertsRepository) GetByID(ctx context.Context, id string) (*models.Alert, error) {
	var a models.Alert
	err := r.db.QueryRowContext(ctx,
		`SELECT id, organization_id, rule_id, workspace_name, severity, title, description,
		        metadata, is_acknowledged, acknowledged_by, acknowledged_at, created_at
		 FROM alerts
		 WHERE id = $1`,
		id,
	).Scan(
		&a.ID,
		&a.OrganizationID,
		&a.RuleID,
		&a.WorkspaceName,
		&a.Severity,
		&a.Title,
		&a.Description,
		&a.Metadata,
		&a.IsAcknowledged,
		&a.AcknowledgedBy,
		&a.AcknowledgedAt,
		&a.CreatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get alert by ID: %w", err)
	}
	return &a, nil
}

// ListByOrganization returns paginated alerts for a given organization, along with the total count.
// Optional filters: severity (empty string to skip), acknowledged pointer (nil to skip).
func (r *AlertsRepository) ListByOrganization(ctx context.Context, orgID string, severity string, acknowledged *bool, limit, offset int) ([]models.Alert, int, error) {
	// Build dynamic WHERE clause.
	query := "SELECT COUNT(*) FROM alerts WHERE organization_id = $1"
	args := []interface{}{orgID}
	argIdx := 2

	if severity != "" {
		query += fmt.Sprintf(" AND severity = $%d", argIdx)
		args = append(args, severity)
		argIdx++
	}
	if acknowledged != nil {
		query += fmt.Sprintf(" AND is_acknowledged = $%d", argIdx)
		args = append(args, *acknowledged)
		// argIdx not incremented here; count query is complete.
	}

	var total int
	err := r.db.QueryRowContext(ctx, query, args...).Scan(&total)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to count alerts: %w", err)
	}

	selectQuery := `SELECT id, organization_id, rule_id, workspace_name, severity, title, description,
		        metadata, is_acknowledged, acknowledged_by, acknowledged_at, created_at
		 FROM alerts
		 WHERE organization_id = $1`

	selectArgs := []interface{}{orgID}
	selectIdx := 2

	if severity != "" {
		selectQuery += fmt.Sprintf(" AND severity = $%d", selectIdx)
		selectArgs = append(selectArgs, severity)
		selectIdx++
	}
	if acknowledged != nil {
		selectQuery += fmt.Sprintf(" AND is_acknowledged = $%d", selectIdx)
		selectArgs = append(selectArgs, *acknowledged)
		selectIdx++
	}

	selectQuery += fmt.Sprintf(" ORDER BY created_at DESC LIMIT $%d OFFSET $%d", selectIdx, selectIdx+1)
	selectArgs = append(selectArgs, limit, offset)

	rows, err := r.db.QueryContext(ctx, selectQuery, selectArgs...)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to list alerts: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var alerts []models.Alert
	for rows.Next() {
		var a models.Alert
		if err := rows.Scan(
			&a.ID,
			&a.OrganizationID,
			&a.RuleID,
			&a.WorkspaceName,
			&a.Severity,
			&a.Title,
			&a.Description,
			&a.Metadata,
			&a.IsAcknowledged,
			&a.AcknowledgedBy,
			&a.AcknowledgedAt,
			&a.CreatedAt,
		); err != nil {
			return nil, 0, fmt.Errorf("failed to scan alert: %w", err)
		}
		alerts = append(alerts, a)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("failed to iterate alerts: %w", err)
	}
	return alerts, total, nil
}

// Acknowledge marks an alert as acknowledged by a user.
func (r *AlertsRepository) Acknowledge(ctx context.Context, id string, userID string) error {
	now := time.Now()
	_, err := r.db.ExecContext(ctx,
		`UPDATE alerts
		 SET is_acknowledged = true, acknowledged_by = $1, acknowledged_at = $2
		 WHERE id = $3`,
		userID,
		now,
		id,
	)
	if err != nil {
		return fmt.Errorf("failed to acknowledge alert: %w", err)
	}
	return nil
}

// ListUnacknowledged returns all unacknowledged alerts for a given organization.
func (r *AlertsRepository) ListUnacknowledged(ctx context.Context, orgID string) ([]models.Alert, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, organization_id, rule_id, workspace_name, severity, title, description,
		        metadata, is_acknowledged, acknowledged_by, acknowledged_at, created_at
		 FROM alerts
		 WHERE organization_id = $1 AND is_acknowledged = false
		 ORDER BY created_at DESC`,
		orgID,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to list unacknowledged alerts: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var alerts []models.Alert
	for rows.Next() {
		var a models.Alert
		if err := rows.Scan(
			&a.ID,
			&a.OrganizationID,
			&a.RuleID,
			&a.WorkspaceName,
			&a.Severity,
			&a.Title,
			&a.Description,
			&a.Metadata,
			&a.IsAcknowledged,
			&a.AcknowledgedBy,
			&a.AcknowledgedAt,
			&a.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("failed to scan unacknowledged alert: %w", err)
		}
		alerts = append(alerts, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to iterate unacknowledged alerts: %w", err)
	}
	return alerts, nil
}
