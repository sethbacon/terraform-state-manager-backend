package repositories

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/terraform-state-manager/terraform-state-manager/internal/db/models"
)

// StateSnapshotRepository handles database operations for state snapshots.
type StateSnapshotRepository struct {
	db *sql.DB
}

// NewStateSnapshotRepository creates a new StateSnapshotRepository.
func NewStateSnapshotRepository(db *sql.DB) *StateSnapshotRepository {
	return &StateSnapshotRepository{db: db}
}

// Create inserts a new state snapshot into the database.
func (r *StateSnapshotRepository) Create(ctx context.Context, snapshot *models.StateSnapshot) error {
	err := r.db.QueryRowContext(ctx,
		`INSERT INTO state_snapshots (organization_id, source_id, workspace_name, workspace_id,
		        snapshot_data, resource_count, rum_count, terraform_version, state_serial)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		 RETURNING id, captured_at`,
		snapshot.OrganizationID,
		snapshot.SourceID,
		snapshot.WorkspaceName,
		snapshot.WorkspaceID,
		snapshot.SnapshotData,
		snapshot.ResourceCount,
		snapshot.RUMCount,
		snapshot.TerraformVersion,
		snapshot.StateSerial,
	).Scan(&snapshot.ID, &snapshot.CapturedAt)
	if err != nil {
		return fmt.Errorf("failed to create state snapshot: %w", err)
	}
	return nil
}

// GetByID retrieves a state snapshot by its ID.
func (r *StateSnapshotRepository) GetByID(ctx context.Context, id string) (*models.StateSnapshot, error) {
	var s models.StateSnapshot
	err := r.db.QueryRowContext(ctx,
		`SELECT id, organization_id, source_id, workspace_name, workspace_id,
		        snapshot_data, resource_count, rum_count, terraform_version,
		        state_serial, captured_at
		 FROM state_snapshots
		 WHERE id = $1`,
		id,
	).Scan(
		&s.ID,
		&s.OrganizationID,
		&s.SourceID,
		&s.WorkspaceName,
		&s.WorkspaceID,
		&s.SnapshotData,
		&s.ResourceCount,
		&s.RUMCount,
		&s.TerraformVersion,
		&s.StateSerial,
		&s.CapturedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get state snapshot by ID: %w", err)
	}
	return &s, nil
}

// ListByWorkspace returns paginated snapshots for a given workspace name, ordered
// by captured_at descending, along with the total count.
func (r *StateSnapshotRepository) ListByWorkspace(ctx context.Context, orgID, workspaceName string, limit, offset int) ([]models.StateSnapshot, int, error) {
	var total int
	err := r.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM state_snapshots WHERE organization_id = $1 AND workspace_name = $2",
		orgID, workspaceName,
	).Scan(&total)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to count state snapshots: %w", err)
	}

	rows, err := r.db.QueryContext(ctx,
		`SELECT id, organization_id, source_id, workspace_name, workspace_id,
		        snapshot_data, resource_count, rum_count, terraform_version,
		        state_serial, captured_at
		 FROM state_snapshots
		 WHERE organization_id = $1 AND workspace_name = $2
		 ORDER BY captured_at DESC
		 LIMIT $3 OFFSET $4`,
		orgID, workspaceName, limit, offset,
	)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to list state snapshots: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var snapshots []models.StateSnapshot
	for rows.Next() {
		var s models.StateSnapshot
		if err := rows.Scan(
			&s.ID,
			&s.OrganizationID,
			&s.SourceID,
			&s.WorkspaceName,
			&s.WorkspaceID,
			&s.SnapshotData,
			&s.ResourceCount,
			&s.RUMCount,
			&s.TerraformVersion,
			&s.StateSerial,
			&s.CapturedAt,
		); err != nil {
			return nil, 0, fmt.Errorf("failed to scan state snapshot: %w", err)
		}
		snapshots = append(snapshots, s)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("failed to iterate state snapshots: %w", err)
	}
	return snapshots, total, nil
}

// ListByOrganization returns paginated snapshots for an organization, ordered by
// captured_at descending, along with the total count.
func (r *StateSnapshotRepository) ListByOrganization(ctx context.Context, orgID string, limit, offset int) ([]models.StateSnapshot, int, error) {
	var total int
	err := r.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM state_snapshots WHERE organization_id = $1",
		orgID,
	).Scan(&total)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to count state snapshots: %w", err)
	}

	rows, err := r.db.QueryContext(ctx,
		`SELECT id, organization_id, source_id, workspace_name, workspace_id,
		        snapshot_data, resource_count, rum_count, terraform_version,
		        state_serial, captured_at
		 FROM state_snapshots
		 WHERE organization_id = $1
		 ORDER BY captured_at DESC
		 LIMIT $2 OFFSET $3`,
		orgID, limit, offset,
	)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to list state snapshots: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var snapshots []models.StateSnapshot
	for rows.Next() {
		var s models.StateSnapshot
		if err := rows.Scan(
			&s.ID,
			&s.OrganizationID,
			&s.SourceID,
			&s.WorkspaceName,
			&s.WorkspaceID,
			&s.SnapshotData,
			&s.ResourceCount,
			&s.RUMCount,
			&s.TerraformVersion,
			&s.StateSerial,
			&s.CapturedAt,
		); err != nil {
			return nil, 0, fmt.Errorf("failed to scan state snapshot: %w", err)
		}
		snapshots = append(snapshots, s)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("failed to iterate state snapshots: %w", err)
	}
	return snapshots, total, nil
}

// GetLatestForWorkspace returns the most recent snapshot for the given workspace
// within an organization.
func (r *StateSnapshotRepository) GetLatestForWorkspace(ctx context.Context, orgID, workspaceName string) (*models.StateSnapshot, error) {
	var s models.StateSnapshot
	err := r.db.QueryRowContext(ctx,
		`SELECT id, organization_id, source_id, workspace_name, workspace_id,
		        snapshot_data, resource_count, rum_count, terraform_version,
		        state_serial, captured_at
		 FROM state_snapshots
		 WHERE organization_id = $1 AND workspace_name = $2
		 ORDER BY captured_at DESC
		 LIMIT 1`,
		orgID, workspaceName,
	).Scan(
		&s.ID,
		&s.OrganizationID,
		&s.SourceID,
		&s.WorkspaceName,
		&s.WorkspaceID,
		&s.SnapshotData,
		&s.ResourceCount,
		&s.RUMCount,
		&s.TerraformVersion,
		&s.StateSerial,
		&s.CapturedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get latest snapshot for workspace: %w", err)
	}
	return &s, nil
}

// DeleteOlderThan removes all snapshots for a given organization that were captured
// before the specified cutoff time. Returns the number of deleted rows.
func (r *StateSnapshotRepository) DeleteOlderThan(ctx context.Context, orgID string, cutoff time.Time) (int64, error) {
	result, err := r.db.ExecContext(ctx,
		"DELETE FROM state_snapshots WHERE organization_id = $1 AND captured_at < $2",
		orgID, cutoff,
	)
	if err != nil {
		return 0, fmt.Errorf("failed to delete old snapshots: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("failed to get deleted snapshot count: %w", err)
	}
	return count, nil
}
