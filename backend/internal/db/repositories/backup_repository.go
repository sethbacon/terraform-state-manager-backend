package repositories

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/terraform-state-manager/terraform-state-manager/internal/db/models"
)

// BackupRepository handles database operations for state backups.
type BackupRepository struct {
	db *sql.DB
}

// NewBackupRepository creates a new BackupRepository.
func NewBackupRepository(db *sql.DB) *BackupRepository {
	return &BackupRepository{db: db}
}

// Create inserts a new state backup into the database.
func (r *BackupRepository) Create(ctx context.Context, backup *models.StateBackup) error {
	err := r.db.QueryRowContext(ctx,
		`INSERT INTO state_backups (
			organization_id, source_id, workspace_name, workspace_id,
			storage_backend, storage_path, file_size_bytes, terraform_version,
			state_serial, checksum_sha256, retention_policy_id, expires_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		 RETURNING id, created_at`,
		backup.OrganizationID,
		backup.SourceID,
		backup.WorkspaceName,
		backup.WorkspaceID,
		backup.StorageBackend,
		backup.StoragePath,
		backup.FileSizeBytes,
		backup.TerraformVersion,
		backup.StateSerial,
		backup.ChecksumSHA256,
		backup.RetentionPolicyID,
		backup.ExpiresAt,
	).Scan(&backup.ID, &backup.CreatedAt)
	if err != nil {
		return fmt.Errorf("failed to create state backup: %w", err)
	}
	return nil
}

// GetByID retrieves a state backup by its ID.
func (r *BackupRepository) GetByID(ctx context.Context, id string) (*models.StateBackup, error) {
	var b models.StateBackup
	err := r.db.QueryRowContext(ctx,
		`SELECT id, organization_id, source_id, workspace_name, workspace_id,
		        storage_backend, storage_path, file_size_bytes, terraform_version,
		        state_serial, checksum_sha256, retention_policy_id, expires_at, created_at
		 FROM state_backups
		 WHERE id = $1`,
		id,
	).Scan(
		&b.ID,
		&b.OrganizationID,
		&b.SourceID,
		&b.WorkspaceName,
		&b.WorkspaceID,
		&b.StorageBackend,
		&b.StoragePath,
		&b.FileSizeBytes,
		&b.TerraformVersion,
		&b.StateSerial,
		&b.ChecksumSHA256,
		&b.RetentionPolicyID,
		&b.ExpiresAt,
		&b.CreatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get state backup by ID: %w", err)
	}
	return &b, nil
}

// ListByOrganization returns paginated state backups for a given organization, along with the total count.
func (r *BackupRepository) ListByOrganization(ctx context.Context, orgID string, limit, offset int) ([]models.StateBackup, int, error) {
	var total int
	err := r.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM state_backups WHERE organization_id = $1",
		orgID,
	).Scan(&total)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to count state backups: %w", err)
	}

	rows, err := r.db.QueryContext(ctx,
		`SELECT id, organization_id, source_id, workspace_name, workspace_id,
		        storage_backend, storage_path, file_size_bytes, terraform_version,
		        state_serial, checksum_sha256, retention_policy_id, expires_at, created_at
		 FROM state_backups
		 WHERE organization_id = $1
		 ORDER BY created_at DESC
		 LIMIT $2 OFFSET $3`,
		orgID, limit, offset,
	)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to list state backups: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var backups []models.StateBackup
	for rows.Next() {
		var b models.StateBackup
		if err := rows.Scan(
			&b.ID,
			&b.OrganizationID,
			&b.SourceID,
			&b.WorkspaceName,
			&b.WorkspaceID,
			&b.StorageBackend,
			&b.StoragePath,
			&b.FileSizeBytes,
			&b.TerraformVersion,
			&b.StateSerial,
			&b.ChecksumSHA256,
			&b.RetentionPolicyID,
			&b.ExpiresAt,
			&b.CreatedAt,
		); err != nil {
			return nil, 0, fmt.Errorf("failed to scan state backup: %w", err)
		}
		backups = append(backups, b)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("failed to iterate state backups: %w", err)
	}
	return backups, total, nil
}

// ListByWorkspace returns all state backups for a given workspace name within an organization.
func (r *BackupRepository) ListByWorkspace(ctx context.Context, orgID, workspaceName string) ([]models.StateBackup, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, organization_id, source_id, workspace_name, workspace_id,
		        storage_backend, storage_path, file_size_bytes, terraform_version,
		        state_serial, checksum_sha256, retention_policy_id, expires_at, created_at
		 FROM state_backups
		 WHERE organization_id = $1 AND workspace_name = $2
		 ORDER BY created_at DESC`,
		orgID, workspaceName,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to list backups by workspace: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var backups []models.StateBackup
	for rows.Next() {
		var b models.StateBackup
		if err := rows.Scan(
			&b.ID,
			&b.OrganizationID,
			&b.SourceID,
			&b.WorkspaceName,
			&b.WorkspaceID,
			&b.StorageBackend,
			&b.StoragePath,
			&b.FileSizeBytes,
			&b.TerraformVersion,
			&b.StateSerial,
			&b.ChecksumSHA256,
			&b.RetentionPolicyID,
			&b.ExpiresAt,
			&b.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("failed to scan state backup: %w", err)
		}
		backups = append(backups, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to iterate workspace backups: %w", err)
	}
	return backups, nil
}

// Delete removes a state backup by its ID.
func (r *BackupRepository) Delete(ctx context.Context, id string) error {
	_, err := r.db.ExecContext(ctx, "DELETE FROM state_backups WHERE id = $1", id)
	if err != nil {
		return fmt.Errorf("failed to delete state backup: %w", err)
	}
	return nil
}

// GetExpired returns all state backups whose expires_at timestamp is in the past.
func (r *BackupRepository) GetExpired(ctx context.Context) ([]models.StateBackup, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, organization_id, source_id, workspace_name, workspace_id,
		        storage_backend, storage_path, file_size_bytes, terraform_version,
		        state_serial, checksum_sha256, retention_policy_id, expires_at, created_at
		 FROM state_backups
		 WHERE expires_at IS NOT NULL AND expires_at < $1
		 ORDER BY expires_at ASC`,
		time.Now(),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to get expired backups: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var backups []models.StateBackup
	for rows.Next() {
		var b models.StateBackup
		if err := rows.Scan(
			&b.ID,
			&b.OrganizationID,
			&b.SourceID,
			&b.WorkspaceName,
			&b.WorkspaceID,
			&b.StorageBackend,
			&b.StoragePath,
			&b.FileSizeBytes,
			&b.TerraformVersion,
			&b.StateSerial,
			&b.ChecksumSHA256,
			&b.RetentionPolicyID,
			&b.ExpiresAt,
			&b.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("failed to scan expired backup: %w", err)
		}
		backups = append(backups, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to iterate expired backups: %w", err)
	}
	return backups, nil
}

// CountByWorkspace returns the number of backups for a workspace within an organization.
func (r *BackupRepository) CountByWorkspace(ctx context.Context, orgID, workspaceName string) (int, error) {
	var count int
	err := r.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM state_backups WHERE organization_id = $1 AND workspace_name = $2",
		orgID, workspaceName,
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("failed to count backups by workspace: %w", err)
	}
	return count, nil
}
