// Package backup implements the state backup and restore service, including
// integrity verification via SHA-256 checksums and retention policy enforcement.
package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"time"

	"github.com/terraform-state-manager/terraform-state-manager/internal/db/models"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
	"github.com/terraform-state-manager/terraform-state-manager/internal/storage"
)

// Service provides state backup operations including creation, restoration,
// integrity verification, and retention policy enforcement.
type Service struct {
	backupRepo    *repositories.BackupRepository
	retentionRepo *repositories.RetentionPolicyRepository
	sourceRepo    *repositories.StateSourceRepository
	storage       storage.Backend
}

// NewService creates a new backup Service.
func NewService(
	backupRepo *repositories.BackupRepository,
	retentionRepo *repositories.RetentionPolicyRepository,
	sourceRepo *repositories.StateSourceRepository,
	storageBackend storage.Backend,
) *Service {
	return &Service{
		backupRepo:    backupRepo,
		retentionRepo: retentionRepo,
		sourceRepo:    sourceRepo,
		storage:       storageBackend,
	}
}

// CreateBackup stores a state file as a backup, computes its SHA-256 checksum,
// persists metadata to the database, and applies the default retention policy
// expiration if one exists.
func (s *Service) CreateBackup(
	ctx context.Context,
	orgID string,
	sourceID string,
	workspaceName string,
	workspaceID string,
	stateData []byte,
	tfVersion string,
	serial int,
) (*models.StateBackup, error) {
	// Compute SHA-256 checksum.
	hash := sha256.Sum256(stateData)
	checksum := hex.EncodeToString(hash[:])

	// Build storage path: backups/{org_id}/{workspace_name}/{timestamp}.tfstate
	timestamp := time.Now().UTC().Format("20060102T150405Z")
	storagePath := fmt.Sprintf("backups/%s/%s/%s.tfstate", orgID, workspaceName, timestamp)

	// Write state data to storage.
	if err := s.storage.Put(ctx, storagePath, stateData); err != nil {
		return nil, fmt.Errorf("failed to store backup data: %w", err)
	}

	// Determine expiration from the default retention policy.
	var expiresAt *time.Time
	var retentionPolicyID *string
	defaultPolicy, err := s.retentionRepo.GetDefault(ctx, orgID)
	if err != nil {
		slog.Warn("Failed to get default retention policy; backup will not expire",
			"org_id", orgID, "error", err)
	}
	if defaultPolicy != nil {
		retentionPolicyID = &defaultPolicy.ID
		if defaultPolicy.MaxAgeDays != nil && *defaultPolicy.MaxAgeDays > 0 {
			exp := time.Now().AddDate(0, 0, *defaultPolicy.MaxAgeDays)
			expiresAt = &exp
		}
	}

	// Build the backup record.
	fileSize := int64(len(stateData))
	backup := &models.StateBackup{
		OrganizationID:    orgID,
		SourceID:          nilIfEmpty(sourceID),
		WorkspaceName:     workspaceName,
		WorkspaceID:       nilIfEmpty(workspaceID),
		StorageBackend:    "default",
		StoragePath:       storagePath,
		FileSizeBytes:     &fileSize,
		TerraformVersion:  nilIfEmpty(tfVersion),
		StateSerial:       &serial,
		ChecksumSHA256:    &checksum,
		RetentionPolicyID: retentionPolicyID,
		ExpiresAt:         expiresAt,
	}

	if err := s.backupRepo.Create(ctx, backup); err != nil {
		// Best effort: clean up the stored file.
		_ = s.storage.Delete(ctx, storagePath)
		return nil, fmt.Errorf("failed to create backup record: %w", err)
	}

	slog.Info("State backup created",
		"backup_id", backup.ID,
		"workspace", workspaceName,
		"size_bytes", fileSize,
		"checksum", checksum)

	return backup, nil
}

// RestoreBackup retrieves the raw state data for a previously created backup.
func (s *Service) RestoreBackup(ctx context.Context, backupID string) ([]byte, *models.StateBackup, error) {
	backup, err := s.backupRepo.GetByID(ctx, backupID)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to retrieve backup record: %w", err)
	}
	if backup == nil {
		return nil, nil, fmt.Errorf("backup not found: %s", backupID)
	}

	data, err := s.storage.Get(ctx, backup.StoragePath)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to retrieve backup data from storage: %w", err)
	}

	slog.Info("State backup restored",
		"backup_id", backupID,
		"workspace", backup.WorkspaceName,
		"size_bytes", len(data))

	return data, backup, nil
}

// VerifyBackup re-computes the SHA-256 checksum of the stored state data and
// compares it against the recorded checksum to verify data integrity.
func (s *Service) VerifyBackup(ctx context.Context, backupID string) (bool, error) {
	backup, err := s.backupRepo.GetByID(ctx, backupID)
	if err != nil {
		return false, fmt.Errorf("failed to retrieve backup record: %w", err)
	}
	if backup == nil {
		return false, fmt.Errorf("backup not found: %s", backupID)
	}

	if backup.ChecksumSHA256 == nil {
		return false, fmt.Errorf("backup %s has no stored checksum", backupID)
	}

	data, err := s.storage.Get(ctx, backup.StoragePath)
	if err != nil {
		return false, fmt.Errorf("failed to retrieve backup data from storage: %w", err)
	}

	hash := sha256.Sum256(data)
	computed := hex.EncodeToString(hash[:])

	valid := computed == *backup.ChecksumSHA256
	if !valid {
		slog.Warn("Backup integrity check failed",
			"backup_id", backupID,
			"expected", *backup.ChecksumSHA256,
			"computed", computed)
	} else {
		slog.Info("Backup integrity check passed",
			"backup_id", backupID,
			"checksum", computed)
	}

	return valid, nil
}

// DeleteBackup removes a backup from both storage and the database.
func (s *Service) DeleteBackup(ctx context.Context, backupID string) error {
	backup, err := s.backupRepo.GetByID(ctx, backupID)
	if err != nil {
		return fmt.Errorf("failed to retrieve backup record: %w", err)
	}
	if backup == nil {
		return fmt.Errorf("backup not found: %s", backupID)
	}

	// Delete from storage first (best-effort), then from DB.
	if err := s.storage.Delete(ctx, backup.StoragePath); err != nil {
		slog.Warn("Failed to delete backup file from storage (will still remove DB record)",
			"backup_id", backupID, "path", backup.StoragePath, "error", err)
	}

	if err := s.backupRepo.Delete(ctx, backupID); err != nil {
		return fmt.Errorf("failed to delete backup record: %w", err)
	}

	slog.Info("State backup deleted", "backup_id", backupID, "path", backup.StoragePath)
	return nil
}

// ApplyRetention enforces all retention policies for the given organization.
// It delegates to EnforceRetention in the retention service logic.
func (s *Service) ApplyRetention(ctx context.Context, orgID string) error {
	return EnforceRetention(ctx, orgID, s.retentionRepo, s.backupRepo, s.storage)
}

// nilIfEmpty returns a pointer to s if non-empty, otherwise nil.
func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
