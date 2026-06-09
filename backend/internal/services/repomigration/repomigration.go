// Package repomigration implements the Azure DevOps repo-migration EXECUTE path.
// It consumes the read-only dry-run plan produced by ado.EnumerateMigrationPlan
// and provisions a target ADO project over REST, idempotently and resumably.
//
// This is deliberately separate from services/migration, which moves Terraform
// state files between storage backends — a different concern. The orchestrator
// here provisions ADO resources in dependency order: repository → pipelines →
// branch policies → variable groups → service connections.
//
// Idempotency: every resource is recorded as a checkpoint step. Before creating
// a resource the orchestrator skips it if a terminal checkpoint already exists
// (resume), and a 409 Conflict from ADO (the resource already exists in the
// target) is treated as an idempotent success (skipped-exists). Re-running an
// already-completed migration is therefore a no-op.
//
// Resumability: per-resource progress is persisted through a CheckpointStore so
// an interrupted run resumes where it stopped rather than restarting.
//
// DEFERRED: the actual git history/refs push (clone source → push to target) is
// out of scope for this REST slice. It plugs in at GitHistoryPusher; the default
// is a no-op, so repositories are provisioned empty until that slice lands.
package repomigration

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/terraform-state-manager/terraform-state-manager/internal/clients/ado"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/models"
)

// defaultPipelineYAMLPath is the YAML definition path assumed for adopted
// pipelines. The dry-run plan does not capture each pipeline's backing YAML
// path, so a conventional default is used; a later slice can enrich the plan to
// carry the real path per pipeline.
const defaultPipelineYAMLPath = "azure-pipelines.yml"

// TargetClient is the subset of the ADO write surface the orchestrator needs.
// Defining it as an interface (satisfied by *ado.Client) lets tests drive the
// orchestrator with a fake that records calls and injects conflicts/errors,
// while production passes a real client pointed at the target org/project.
type TargetClient interface {
	CreateRepository(ctx context.Context, name string) (*ado.Repository, error)
	CreatePipeline(ctx context.Context, req ado.CreatePipelineRequest) (*ado.Pipeline, error)
	AdoptBranchPolicy(ctx context.Context, req ado.AdoptBranchPolicyRequest) (*ado.BranchPolicy, error)
	AdoptVariableGroup(ctx context.Context, name string, variableNames []string) (*ado.VariableGroup, error)
	AdoptServiceConnection(ctx context.Context, req ado.AdoptServiceConnectionRequest) (*ado.ServiceConnection, error)
}

// Service orchestrates a repo-migration EXECUTE run.
type Service struct {
	store      CheckpointStore
	gitHistory GitHistoryPusher
	// gitHistoryActive is true when a real pusher was supplied (not the no-op
	// default). When active the orchestrator records a distinct git_history
	// checkpoint step per repository and counts it in the run total; with the
	// no-op default no such step is recorded, so repositories are simply
	// provisioned empty and the resource total is unchanged.
	gitHistoryActive bool
	logger           *slog.Logger
}

// NewService creates an execute orchestrator. If gitHistory is nil the deferred
// git-history step is a no-op (repositories are provisioned empty) and no
// git_history checkpoint is recorded. Supplying a real pusher (e.g.
// NewGoGitPusher) activates the git-history step. If logger is nil the default
// slog logger is used.
func NewService(store CheckpointStore, gitHistory GitHistoryPusher, logger *slog.Logger) *Service {
	active := gitHistory != nil
	if gitHistory == nil {
		gitHistory = noopGitHistoryPusher{}
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{store: store, gitHistory: gitHistory, gitHistoryActive: active, logger: logger}
}

// ResourceResult is the per-resource outcome surfaced in an ExecuteSummary.
type ResourceResult struct {
	ResourceType string `json:"resource_type"`
	ResourceKey  string `json:"resource_key"`
	Status       string `json:"status"` // created | skipped-exists | failed
	Detail       string `json:"detail,omitempty"`
	Error        string `json:"error,omitempty"`
}

// ExecuteSummary aggregates the outcome of an Execute run.
type ExecuteSummary struct {
	MigrationID string           `json:"migration_id"`
	Status      string           `json:"status"`
	Total       int              `json:"total"`
	Created     int              `json:"created"`
	Skipped     int              `json:"skipped"`
	Failed      int              `json:"failed"`
	Results     []ResourceResult `json:"results"`
}

// Execute provisions the target project described by plan, using target to make
// the ADO write calls and migrationID to scope checkpoint state. It is safe to
// re-run: resources with an existing terminal checkpoint are skipped, and ADO
// 409 Conflicts are treated as idempotent successes. Per-resource failures are
// recorded and do not abort the remaining resources; the run's final status is
// "failed" if any resource failed, otherwise "completed".
func (s *Service) Execute(ctx context.Context, migrationID string, plan *ado.MigrationPlan, target TargetClient) (*ExecuteSummary, error) {
	migration, err := s.store.GetMigration(ctx, migrationID)
	if err != nil {
		return nil, fmt.Errorf("loading repo migration: %w", err)
	}
	if migration == nil {
		return nil, fmt.Errorf("repo migration not found: %s", migrationID)
	}

	// Load prior checkpoints so a resumed run skips already-terminal steps.
	priorSteps, err := s.store.ListSteps(ctx, migrationID)
	if err != nil {
		return nil, fmt.Errorf("loading checkpoint steps: %w", err)
	}
	done := terminalStepIndex(priorSteps)

	now := time.Now()
	migration.Status = models.RepoMigrationStatusRunning
	if migration.StartedAt == nil {
		migration.StartedAt = &now
	}
	migration.TotalResources = totalResources(plan)
	if s.gitHistoryActive {
		// Each repository contributes an extra git_history step when a real pusher
		// is active.
		migration.TotalResources += len(plan.Repositories)
	}
	if err := s.store.UpdateMigration(ctx, migration); err != nil {
		s.logger.Warn("failed to mark repo migration running", "migration_id", migrationID, "error", err)
	}

	summary := &ExecuteSummary{MigrationID: migrationID, Total: migration.TotalResources}

	// repoIDs maps a repository name to its id in the target project, populated as
	// repositories are created so pipelines can reference their backing repo.
	repoIDs := map[string]string{}

	// 1. Repositories, each followed by its git-history push (when active).
	for _, repo := range plan.Repositories {
		res, created := s.ensureRepository(ctx, migrationID, target, repo, done, repoIDs)
		summary.record(res)
		if s.gitHistoryActive {
			summary.record(s.ensureGitHistory(ctx, migrationID, repo, created, done))
		}
	}

	// 2. Pipelines — referenced against a target repository.
	for _, pipeline := range plan.Pipelines {
		res := s.ensurePipeline(ctx, migrationID, target, pipeline, done, repoIDs)
		summary.record(res)
	}

	// 3. Branch policies.
	for _, policy := range plan.BranchPolicies {
		res := s.ensureBranchPolicy(ctx, migrationID, target, policy, done)
		summary.record(res)
	}

	// 4. Variable groups.
	for _, group := range plan.VariableGroups {
		res := s.ensureVariableGroup(ctx, migrationID, target, group, done)
		summary.record(res)
	}

	// 5. Service connections.
	for _, conn := range plan.ServiceConnections {
		res := s.ensureServiceConnection(ctx, migrationID, target, conn, done)
		summary.record(res)
	}

	// Finalize the run.
	completed := time.Now()
	migration.CreatedResources = summary.Created
	migration.SkippedResources = summary.Skipped
	migration.FailedResources = summary.Failed
	migration.CompletedAt = &completed
	if summary.Failed > 0 {
		migration.Status = models.RepoMigrationStatusFailed
	} else {
		migration.Status = models.RepoMigrationStatusCompleted
	}
	summary.Status = migration.Status
	if err := s.store.UpdateMigration(ctx, migration); err != nil {
		return summary, fmt.Errorf("finalizing repo migration: %w", err)
	}

	s.logger.Info("repo migration execute finished",
		"migration_id", migrationID,
		"status", summary.Status,
		"created", summary.Created,
		"skipped", summary.Skipped,
		"failed", summary.Failed)

	return summary, nil
}

// ensureRepository creates the target repository (or skips an existing one). It
// returns the per-resource result and, when a repository was freshly created in
// this run, the created *ado.Repository (carrying its id and push URL) so the
// caller can run the git-history step against it. The pointer is nil when the
// repository was skipped (resumed/conflict) or failed, in which case no history
// push is attempted.
func (s *Service) ensureRepository(ctx context.Context, migrationID string, target TargetClient, repo ado.Repository, done map[stepKey]string, repoIDs map[string]string) (ResourceResult, *ado.Repository) {
	key := stepKey{models.RepoMigrationResourceRepository, repo.Name}
	if prior, ok := done[key]; ok {
		return s.skipResume(ctx, migrationID, key, prior), nil
	}

	created, err := target.CreateRepository(ctx, repo.Name)
	if err != nil {
		if ado.IsConflict(err) {
			return s.recordSkippedExists(ctx, migrationID, key, "repository already exists in target"), nil
		}
		return s.recordFailed(ctx, migrationID, key, err), nil
	}

	repoIDs[repo.Name] = created.ID
	return s.recordCreated(ctx, migrationID, key, fmt.Sprintf("repository id %s", created.ID)), created
}

// ensureGitHistory pushes the source repository's git history into the freshly
// created target repository via the configured GitHistoryPusher. It is only
// invoked when a real pusher is active (gitHistoryActive). The step is
// checkpointed under its own resource type so a resumed run can re-run just the
// history transfer; a re-push of already-present refs is a no-op by contract.
//
// When created is nil the backing repository was skipped or failed in this run,
// so there is nothing to push into — the step is recorded as skipped-exists
// (idempotent, not a failure) and the push is not attempted.
func (s *Service) ensureGitHistory(ctx context.Context, migrationID string, source ado.Repository, created *ado.Repository, done map[stepKey]string) ResourceResult {
	key := stepKey{models.RepoMigrationResourceGitHistory, source.Name}
	if prior, ok := done[key]; ok {
		return s.skipResume(ctx, migrationID, key, prior)
	}
	if created == nil {
		return s.recordSkippedExists(ctx, migrationID, key, "repository not created in this run; git history not pushed")
	}

	if err := s.gitHistory.Push(ctx, source, *created); err != nil {
		return s.recordFailed(ctx, migrationID, key, err)
	}
	return s.recordCreated(ctx, migrationID, key, fmt.Sprintf("git history pushed to repository id %s", created.ID))
}

// ensurePipeline defines a pipeline in the target, linked to a target repository.
func (s *Service) ensurePipeline(ctx context.Context, migrationID string, target TargetClient, pipeline ado.Pipeline, done map[stepKey]string, repoIDs map[string]string) ResourceResult {
	key := stepKey{models.RepoMigrationResourcePipeline, pipeline.Name}
	if prior, ok := done[key]; ok {
		return s.skipResume(ctx, migrationID, key, prior)
	}

	repoID := defaultRepoID(repoIDs)
	if repoID == "" {
		return s.recordFailed(ctx, migrationID, key,
			errors.New("no target repository available to back the pipeline"))
	}

	created, err := target.CreatePipeline(ctx, ado.CreatePipelineRequest{
		Name:         pipeline.Name,
		Folder:       pipeline.Folder,
		YAMLPath:     defaultPipelineYAMLPath,
		RepositoryID: repoID,
	})
	if err != nil {
		if ado.IsConflict(err) {
			return s.recordSkippedExists(ctx, migrationID, key, "pipeline already exists in target")
		}
		return s.recordFailed(ctx, migrationID, key, err)
	}
	return s.recordCreated(ctx, migrationID, key, fmt.Sprintf("pipeline id %d", created.ID))
}

// ensureBranchPolicy adopts a branch policy in the target.
func (s *Service) ensureBranchPolicy(ctx context.Context, migrationID string, target TargetClient, policy ado.BranchPolicy, done map[stepKey]string) ResourceResult {
	key := stepKey{models.RepoMigrationResourceBranchPolicy, branchPolicyKey(policy)}
	if prior, ok := done[key]; ok {
		return s.skipResume(ctx, migrationID, key, prior)
	}

	_, err := target.AdoptBranchPolicy(ctx, ado.AdoptBranchPolicyRequest{
		TypeID:     policy.Type,
		IsEnabled:  policy.IsEnabled,
		IsBlocking: true,
	})
	if err != nil {
		if ado.IsConflict(err) {
			return s.recordSkippedExists(ctx, migrationID, key, "branch policy already exists in target")
		}
		return s.recordFailed(ctx, migrationID, key, err)
	}
	return s.recordCreated(ctx, migrationID, key, policy.DisplayName)
}

// ensureVariableGroup adopts a variable group (names only, empty placeholders).
func (s *Service) ensureVariableGroup(ctx context.Context, migrationID string, target TargetClient, group ado.VariableGroup, done map[stepKey]string) ResourceResult {
	key := stepKey{models.RepoMigrationResourceVariableGroup, group.Name}
	if prior, ok := done[key]; ok {
		return s.skipResume(ctx, migrationID, key, prior)
	}

	_, err := target.AdoptVariableGroup(ctx, group.Name, group.VariableNames)
	if err != nil {
		if ado.IsConflict(err) {
			return s.recordSkippedExists(ctx, migrationID, key, "variable group already exists in target")
		}
		return s.recordFailed(ctx, migrationID, key, err)
	}
	return s.recordCreated(ctx, migrationID, key,
		fmt.Sprintf("%d variable name(s); values require out-of-band re-supply", len(group.VariableNames)))
}

// ensureServiceConnection adopts a service connection (no credentials copied).
func (s *Service) ensureServiceConnection(ctx context.Context, migrationID string, target TargetClient, conn ado.ServiceConnection, done map[stepKey]string) ResourceResult {
	key := stepKey{models.RepoMigrationResourceServiceConnection, conn.Name}
	if prior, ok := done[key]; ok {
		return s.skipResume(ctx, migrationID, key, prior)
	}

	_, err := target.AdoptServiceConnection(ctx, ado.AdoptServiceConnectionRequest{
		Name: conn.Name,
		Type: conn.Type,
	})
	if err != nil {
		if ado.IsConflict(err) {
			return s.recordSkippedExists(ctx, migrationID, key, "service connection already exists in target")
		}
		return s.recordFailed(ctx, migrationID, key, err)
	}
	return s.recordCreated(ctx, migrationID, key,
		"connection shell created; credentials require out-of-band re-supply")
}
