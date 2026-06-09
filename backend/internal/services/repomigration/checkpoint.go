package repomigration

import (
	"context"

	"github.com/terraform-state-manager/terraform-state-manager/internal/db/models"
)

// CheckpointStore is the persistence seam the execute orchestrator depends on.
// The production implementation is repositories.RepoMigrationRepository (backed
// by Postgres); tests supply an in-memory fake. Keeping the orchestrator behind
// this interface mirrors the StorageFactory decoupling used by the state-file
// migration service and lets the resumability logic be tested without a DB.
type CheckpointStore interface {
	// GetMigration loads a run by id, returning (nil, nil) when absent.
	GetMigration(ctx context.Context, id string) (*models.RepoMigration, error)
	// UpdateMigration persists run status, counters, and timestamps.
	UpdateMigration(ctx context.Context, m *models.RepoMigration) error
	// ListSteps returns all recorded checkpoint steps for a run.
	ListSteps(ctx context.Context, migrationID string) ([]models.RepoMigrationStep, error)
	// UpsertStep records (insert-or-update) one resource checkpoint, keyed on
	// (migration_id, resource_type, resource_key).
	UpsertStep(ctx context.Context, s *models.RepoMigrationStep) error
}
