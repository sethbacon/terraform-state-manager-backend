package repositories

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/terraform-state-manager/terraform-state-manager/internal/db/models"
)

// DriftEventRepository handles database operations for drift events.
type DriftEventRepository struct {
	db *sql.DB
}

// NewDriftEventRepository creates a new DriftEventRepository.
func NewDriftEventRepository(db *sql.DB) *DriftEventRepository {
	return &DriftEventRepository{db: db}
}

// Create inserts a new drift event into the database.
func (r *DriftEventRepository) Create(ctx context.Context, event *models.DriftEvent) error {
	err := r.db.QueryRowContext(ctx,
		`INSERT INTO drift_events (organization_id, workspace_name, snapshot_before,
		        snapshot_after, changes, severity)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 RETURNING id, detected_at`,
		event.OrganizationID,
		event.WorkspaceName,
		event.SnapshotBefore,
		event.SnapshotAfter,
		event.Changes,
		event.Severity,
	).Scan(&event.ID, &event.DetectedAt)
	if err != nil {
		return fmt.Errorf("failed to create drift event: %w", err)
	}
	return nil
}

// GetByID retrieves a drift event by its ID.
func (r *DriftEventRepository) GetByID(ctx context.Context, id string) (*models.DriftEvent, error) {
	var event models.DriftEvent
	err := r.db.QueryRowContext(ctx,
		`SELECT id, organization_id, workspace_name, snapshot_before,
		        snapshot_after, changes, severity, detected_at
		 FROM drift_events
		 WHERE id = $1`,
		id,
	).Scan(
		&event.ID,
		&event.OrganizationID,
		&event.WorkspaceName,
		&event.SnapshotBefore,
		&event.SnapshotAfter,
		&event.Changes,
		&event.Severity,
		&event.DetectedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get drift event by ID: %w", err)
	}
	return &event, nil
}

// ListByOrganization returns paginated drift events for a given organization,
// ordered by detected_at descending, along with the total count.
func (r *DriftEventRepository) ListByOrganization(ctx context.Context, orgID string, limit, offset int) ([]models.DriftEvent, int, error) {
	var total int
	err := r.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM drift_events WHERE organization_id = $1",
		orgID,
	).Scan(&total)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to count drift events: %w", err)
	}

	rows, err := r.db.QueryContext(ctx,
		`SELECT id, organization_id, workspace_name, snapshot_before,
		        snapshot_after, changes, severity, detected_at
		 FROM drift_events
		 WHERE organization_id = $1
		 ORDER BY detected_at DESC
		 LIMIT $2 OFFSET $3`,
		orgID, limit, offset,
	)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to list drift events: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var events []models.DriftEvent
	for rows.Next() {
		var event models.DriftEvent
		if err := rows.Scan(
			&event.ID,
			&event.OrganizationID,
			&event.WorkspaceName,
			&event.SnapshotBefore,
			&event.SnapshotAfter,
			&event.Changes,
			&event.Severity,
			&event.DetectedAt,
		); err != nil {
			return nil, 0, fmt.Errorf("failed to scan drift event: %w", err)
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("failed to iterate drift events: %w", err)
	}
	return events, total, nil
}

// ListByWorkspace returns paginated drift events for a specific workspace within
// an organization, ordered by detected_at descending, along with the total count.
func (r *DriftEventRepository) ListByWorkspace(ctx context.Context, orgID, workspaceName string, limit, offset int) ([]models.DriftEvent, int, error) {
	var total int
	err := r.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM drift_events WHERE organization_id = $1 AND workspace_name = $2",
		orgID, workspaceName,
	).Scan(&total)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to count drift events by workspace: %w", err)
	}

	rows, err := r.db.QueryContext(ctx,
		`SELECT id, organization_id, workspace_name, snapshot_before,
		        snapshot_after, changes, severity, detected_at
		 FROM drift_events
		 WHERE organization_id = $1 AND workspace_name = $2
		 ORDER BY detected_at DESC
		 LIMIT $3 OFFSET $4`,
		orgID, workspaceName, limit, offset,
	)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to list drift events by workspace: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var events []models.DriftEvent
	for rows.Next() {
		var event models.DriftEvent
		if err := rows.Scan(
			&event.ID,
			&event.OrganizationID,
			&event.WorkspaceName,
			&event.SnapshotBefore,
			&event.SnapshotAfter,
			&event.Changes,
			&event.Severity,
			&event.DetectedAt,
		); err != nil {
			return nil, 0, fmt.Errorf("failed to scan drift event: %w", err)
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("failed to iterate drift events: %w", err)
	}
	return events, total, nil
}
