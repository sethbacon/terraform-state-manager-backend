package backup

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/terraform-state-manager/terraform-state-manager/internal/db/models"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
	"github.com/terraform-state-manager/terraform-state-manager/internal/storage"
)

// EnforceRetention enforces all retention policies for the given organization.
// For each policy it:
//  1. Deletes backups that exceed the maximum age (max_age_days).
//  2. Deletes the oldest backups that exceed the maximum count (max_count)
//     per workspace.
//
// This function can be called by the scheduler or manually via the API.
func EnforceRetention(
	ctx context.Context,
	orgID string,
	retentionRepo *repositories.RetentionPolicyRepository,
	backupRepo *repositories.BackupRepository,
	storageBackend storage.Backend,
) error {
	logger := slog.With("org_id", orgID)

	// 1. Delete all expired backups (expires_at < now) regardless of policy.
	expired, err := backupRepo.GetExpired(ctx)
	if err != nil {
		return fmt.Errorf("failed to get expired backups: %w", err)
	}

	deletedExpired := 0
	for _, b := range expired {
		if b.OrganizationID != orgID {
			continue
		}
		if err := deleteBackup(ctx, b, backupRepo, storageBackend); err != nil {
			logger.Warn("Failed to delete expired backup",
				"backup_id", b.ID, "error", err)
			continue
		}
		deletedExpired++
	}
	if deletedExpired > 0 {
		logger.Info("Deleted expired backups", "count", deletedExpired)
	}

	// 2. Load all retention policies for the org and enforce max_count per workspace.
	policies, err := retentionRepo.ListByOrganization(ctx, orgID)
	if err != nil {
		return fmt.Errorf("failed to list retention policies: %w", err)
	}

	for _, policy := range policies {
		if policy.MaxCount == nil || *policy.MaxCount <= 0 {
			continue
		}

		if err := enforceMaxCount(ctx, orgID, policy, backupRepo, storageBackend); err != nil {
			logger.Warn("Failed to enforce max_count policy",
				"policy_id", policy.ID, "policy_name", policy.Name, "error", err)
		}
	}

	return nil
}

// enforceMaxCount enforces the max_count limit of a retention policy by
// deleting the oldest backups per workspace that exceed the configured count.
func enforceMaxCount(
	ctx context.Context,
	orgID string,
	policy models.RetentionPolicy,
	backupRepo *repositories.BackupRepository,
	storageBackend storage.Backend,
) error {
	if policy.MaxCount == nil || *policy.MaxCount <= 0 {
		return nil
	}
	maxCount := *policy.MaxCount
	logger := slog.With("org_id", orgID, "policy_id", policy.ID, "max_count", maxCount)

	// Get all backups for this org that use this retention policy.
	// We need to group them by workspace and trim the oldest.
	allBackups, _, err := backupRepo.ListByOrganization(ctx, orgID, 10000, 0)
	if err != nil {
		return fmt.Errorf("failed to list backups for retention: %w", err)
	}

	// Group backups by workspace that match this retention policy.
	workspaceBackups := make(map[string][]models.StateBackup)
	for _, b := range allBackups {
		if b.RetentionPolicyID != nil && *b.RetentionPolicyID == policy.ID {
			workspaceBackups[b.WorkspaceName] = append(workspaceBackups[b.WorkspaceName], b)
		}
	}

	totalDeleted := 0
	for wsName, backups := range workspaceBackups {
		if len(backups) <= maxCount {
			continue
		}

		// Sort by created_at descending (newest first).
		sort.Slice(backups, func(i, j int) bool {
			return backups[i].CreatedAt.After(backups[j].CreatedAt)
		})

		// Delete backups beyond the max count (the oldest ones).
		toDelete := backups[maxCount:]
		for _, b := range toDelete {
			if err := deleteBackup(ctx, b, backupRepo, storageBackend); err != nil {
				logger.Warn("Failed to delete backup during count enforcement",
					"backup_id", b.ID, "workspace", wsName, "error", err)
				continue
			}
			totalDeleted++
		}
	}

	if totalDeleted > 0 {
		logger.Info("Deleted backups exceeding max_count", "deleted", totalDeleted)
	}

	return nil
}

// deleteBackup removes a backup from both storage and the database.
func deleteBackup(
	ctx context.Context,
	backup models.StateBackup,
	backupRepo *repositories.BackupRepository,
	storageBackend storage.Backend,
) error {
	// Delete from storage first, then from DB.
	if err := storageBackend.Delete(ctx, backup.StoragePath); err != nil {
		slog.Warn("Failed to delete backup file from storage (will still remove DB record)",
			"backup_id", backup.ID, "path", backup.StoragePath, "error", err)
	}

	if err := backupRepo.Delete(ctx, backup.ID); err != nil {
		return fmt.Errorf("failed to delete backup record: %w", err)
	}

	return nil
}

// CleanupExpiredBackups is a convenience wrapper that deletes all expired
// backups across all organizations. Intended to be called by a global scheduler.
func CleanupExpiredBackups(
	ctx context.Context,
	backupRepo *repositories.BackupRepository,
	storageBackend storage.Backend,
) (int, error) {
	expired, err := backupRepo.GetExpired(ctx)
	if err != nil {
		return 0, fmt.Errorf("failed to get expired backups: %w", err)
	}

	deleted := 0
	for _, b := range expired {
		if err := deleteBackup(ctx, b, backupRepo, storageBackend); err != nil {
			slog.Warn("Failed to delete expired backup during global cleanup",
				"backup_id", b.ID, "error", err)
			continue
		}
		deleted++
	}

	if deleted > 0 {
		slog.Info("Global expired backup cleanup completed",
			"deleted", deleted, "total_expired", len(expired),
			"timestamp", time.Now().UTC())
	}

	return deleted, nil
}
