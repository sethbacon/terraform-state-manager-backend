// Package snapshot provides services for capturing and comparing Terraform state
// snapshots. It converts analysis results into point-in-time snapshots and exposes
// workspace history queries.
package snapshot

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/terraform-state-manager/terraform-state-manager/internal/db/models"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
)

// Service encapsulates the snapshot capture and history retrieval logic.
type Service struct {
	snapshotRepo *repositories.StateSnapshotRepository
	driftRepo    *repositories.DriftEventRepository
}

// NewService creates a new snapshot Service.
func NewService(
	snapshotRepo *repositories.StateSnapshotRepository,
	driftRepo *repositories.DriftEventRepository,
) *Service {
	return &Service{
		snapshotRepo: snapshotRepo,
		driftRepo:    driftRepo,
	}
}

// CaptureFromAnalysisResults builds state snapshots from analysis results and
// persists them. For each successful result, it creates a snapshot containing
// the resource type breakdown and metadata. If a previous snapshot exists for
// the same workspace, drift detection is performed automatically.
func (s *Service) CaptureFromAnalysisResults(
	ctx context.Context,
	orgID string,
	sourceID *string,
	results []models.AnalysisResult,
) ([]models.StateSnapshot, error) {
	logger := slog.With("component", "snapshot_service", "org_id", orgID)

	var snapshots []models.StateSnapshot

	for _, result := range results {
		if result.Status != models.ResultStatusSuccess {
			continue
		}

		// Build the snapshot data from the analysis result.
		snapshotData := models.SnapshotData{
			ResourceCount:   result.TotalResources,
			RUMCount:        result.RUMCount,
			ManagedCount:    result.ManagedCount,
			DataSourceCount: result.DataSourceCount,
		}

		// Parse resource types from the analysis result's resources_by_type field.
		if result.ResourcesByType != nil {
			var resourceTypes map[string]int
			if err := json.Unmarshal(result.ResourcesByType, &resourceTypes); err == nil {
				snapshotData.ResourceTypes = resourceTypes
			}
		}
		if snapshotData.ResourceTypes == nil {
			snapshotData.ResourceTypes = make(map[string]int)
		}

		if result.TerraformVersion != nil {
			snapshotData.TerraformVersion = *result.TerraformVersion
		}
		if result.StateSerial != nil {
			snapshotData.StateSerial = *result.StateSerial
		}

		dataBytes, err := json.Marshal(snapshotData)
		if err != nil {
			logger.Error("Failed to marshal snapshot data",
				"workspace", result.WorkspaceName, "error", err)
			continue
		}

		snapshot := models.StateSnapshot{
			OrganizationID:   orgID,
			SourceID:         sourceID,
			WorkspaceName:    result.WorkspaceName,
			WorkspaceID:      result.WorkspaceID,
			SnapshotData:     dataBytes,
			ResourceCount:    result.TotalResources,
			RUMCount:         result.RUMCount,
			TerraformVersion: result.TerraformVersion,
			StateSerial:      result.StateSerial,
		}

		// Fetch the previous snapshot for drift detection before saving the new one.
		previousSnapshot, err := s.snapshotRepo.GetLatestForWorkspace(ctx, orgID, result.WorkspaceName)
		if err != nil {
			logger.Warn("Failed to fetch previous snapshot for drift detection",
				"workspace", result.WorkspaceName, "error", err)
		}

		// Persist the new snapshot.
		if err := s.snapshotRepo.Create(ctx, &snapshot); err != nil {
			logger.Error("Failed to create snapshot",
				"workspace", result.WorkspaceName, "error", err)
			continue
		}

		// Perform drift detection against the previous snapshot.
		if previousSnapshot != nil {
			driftChanges := DetectDrift(previousSnapshot, &snapshot)
			if driftChanges != nil && driftChanges.HasChanges() {
				changesBytes, marshalErr := json.Marshal(driftChanges)
				if marshalErr != nil {
					logger.Error("Failed to marshal drift changes",
						"workspace", result.WorkspaceName, "error", marshalErr)
				} else {
					severity := models.ClassifyDriftSeverity(
						len(driftChanges.Added),
						len(driftChanges.Removed),
						len(driftChanges.Modified),
					)
					event := &models.DriftEvent{
						OrganizationID: orgID,
						WorkspaceName:  result.WorkspaceName,
						SnapshotBefore: &previousSnapshot.ID,
						SnapshotAfter:  &snapshot.ID,
						Changes:        changesBytes,
						Severity:       severity,
					}
					if createErr := s.driftRepo.Create(ctx, event); createErr != nil {
						logger.Error("Failed to create drift event",
							"workspace", result.WorkspaceName, "error", createErr)
					} else {
						logger.Info("Drift detected",
							"workspace", result.WorkspaceName,
							"severity", severity,
							"added", len(driftChanges.Added),
							"removed", len(driftChanges.Removed),
							"modified", len(driftChanges.Modified))
					}
				}
			}
		}

		snapshots = append(snapshots, snapshot)
	}

	logger.Info("Snapshot capture completed", "captured", len(snapshots), "total_results", len(results))
	return snapshots, nil
}

// GetWorkspaceHistory returns the snapshot history for a workspace, ordered by
// captured_at descending.
func (s *Service) GetWorkspaceHistory(
	ctx context.Context,
	orgID string,
	workspaceName string,
	limit int,
) ([]models.StateSnapshot, int, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	return s.snapshotRepo.ListByWorkspace(ctx, orgID, workspaceName, limit, 0)
}

// CompareSnapshots compares two snapshots and returns the drift analysis.
func (s *Service) CompareSnapshots(
	ctx context.Context,
	beforeID, afterID string,
) (*DriftChanges, error) {
	before, err := s.snapshotRepo.GetByID(ctx, beforeID)
	if err != nil {
		return nil, fmt.Errorf("failed to get before snapshot: %w", err)
	}
	if before == nil {
		return nil, fmt.Errorf("before snapshot not found: %s", beforeID)
	}

	after, err := s.snapshotRepo.GetByID(ctx, afterID)
	if err != nil {
		return nil, fmt.Errorf("failed to get after snapshot: %w", err)
	}
	if after == nil {
		return nil, fmt.Errorf("after snapshot not found: %s", afterID)
	}

	changes := DetectDrift(before, after)
	return changes, nil
}
