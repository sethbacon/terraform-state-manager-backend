package repomigration

import (
	"context"
	"fmt"
	"sort"

	"github.com/terraform-state-manager/terraform-state-manager/internal/clients/ado"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/models"
)

// stepKey identifies a checkpoint step within a run by resource type and key.
type stepKey struct {
	resourceType string
	resourceKey  string
}

// terminalStepIndex builds a lookup of steps that reached a terminal, success-y
// state (created or skipped-exists) in a prior run. The orchestrator skips these
// on resume. Failed/pending steps are intentionally omitted so they are retried.
func terminalStepIndex(steps []models.RepoMigrationStep) map[stepKey]string {
	idx := make(map[stepKey]string, len(steps))
	for _, s := range steps {
		switch s.Status {
		case models.RepoMigrationStepCreated, models.RepoMigrationStepSkippedExists:
			idx[stepKey{s.ResourceType, s.ResourceKey}] = s.Status
		}
	}
	return idx
}

// totalResources counts every resource the plan would provision.
func totalResources(plan *ado.MigrationPlan) int {
	return len(plan.Repositories) +
		len(plan.Pipelines) +
		len(plan.BranchPolicies) +
		len(plan.VariableGroups) +
		len(plan.ServiceConnections)
}

// branchPolicyKey derives a stable checkpoint key for a branch policy. Policies
// have no name; the type id plus display name is stable across re-runs.
func branchPolicyKey(p ado.BranchPolicy) string {
	return fmt.Sprintf("%s:%s", p.Type, p.DisplayName)
}

// defaultRepoID returns a deterministic repository id from the created-repo map
// to back pipelines (the plan does not record each pipeline's repository). It
// returns "" when no repository has been created in the target.
func defaultRepoID(repoIDs map[string]string) string {
	if len(repoIDs) == 0 {
		return ""
	}
	names := make([]string, 0, len(repoIDs))
	for name := range repoIDs {
		names = append(names, name)
	}
	sort.Strings(names)
	return repoIDs[names[0]]
}

// record appends a result to the summary and increments the matching counter.
func (sum *ExecuteSummary) record(res ResourceResult) {
	sum.Results = append(sum.Results, res)
	switch res.Status {
	case models.RepoMigrationStepCreated:
		sum.Created++
	case models.RepoMigrationStepSkippedExists:
		sum.Skipped++
	case models.RepoMigrationStepFailed:
		sum.Failed++
	}
}

// skipResume handles a step already completed in a prior run: it re-asserts the
// prior persisted terminal status (so the checkpoint is unchanged) and returns a
// result counted as skipped for THIS run, without calling ADO. priorStatus is
// surfaced in the detail so the summary remains traceable.
func (s *Service) skipResume(ctx context.Context, migrationID string, key stepKey, priorStatus string) ResourceResult {
	detail := fmt.Sprintf("resumed: already %s in a prior run", priorStatus)
	s.upsert(ctx, migrationID, key, priorStatus, detail, "")
	return ResourceResult{
		ResourceType: key.resourceType,
		ResourceKey:  key.resourceKey,
		Status:       models.RepoMigrationStepSkippedExists,
		Detail:       detail,
	}
}

// recordCreated persists and returns a created result.
func (s *Service) recordCreated(ctx context.Context, migrationID string, key stepKey, detail string) ResourceResult {
	s.upsert(ctx, migrationID, key, models.RepoMigrationStepCreated, detail, "")
	return ResourceResult{
		ResourceType: key.resourceType,
		ResourceKey:  key.resourceKey,
		Status:       models.RepoMigrationStepCreated,
		Detail:       detail,
	}
}

// recordSkippedExists persists and returns a skipped-exists (idempotent) result.
func (s *Service) recordSkippedExists(ctx context.Context, migrationID string, key stepKey, detail string) ResourceResult {
	s.upsert(ctx, migrationID, key, models.RepoMigrationStepSkippedExists, detail, "")
	return ResourceResult{
		ResourceType: key.resourceType,
		ResourceKey:  key.resourceKey,
		Status:       models.RepoMigrationStepSkippedExists,
		Detail:       detail,
	}
}

// recordFailed persists and returns a failed result carrying the error message.
func (s *Service) recordFailed(ctx context.Context, migrationID string, key stepKey, err error) ResourceResult {
	msg := err.Error()
	s.upsert(ctx, migrationID, key, models.RepoMigrationStepFailed, "", msg)
	return ResourceResult{
		ResourceType: key.resourceType,
		ResourceKey:  key.resourceKey,
		Status:       models.RepoMigrationStepFailed,
		Error:        msg,
	}
}

// upsert writes a checkpoint step, logging (but not failing the run on) any
// persistence error so a transient store hiccup does not abort provisioning.
func (s *Service) upsert(ctx context.Context, migrationID string, key stepKey, status, detail, errMsg string) {
	step := &models.RepoMigrationStep{
		MigrationID:  migrationID,
		ResourceType: key.resourceType,
		ResourceKey:  key.resourceKey,
		Status:       status,
	}
	if detail != "" {
		step.Detail = &detail
	}
	if errMsg != "" {
		step.Error = &errMsg
	}
	if err := s.store.UpsertStep(ctx, step); err != nil {
		s.logger.Warn("failed to persist checkpoint step",
			"migration_id", migrationID,
			"resource_type", key.resourceType,
			"resource_key", key.resourceKey,
			"error", err)
	}
}
