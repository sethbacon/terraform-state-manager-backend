package repomigration

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/terraform-state-manager/terraform-state-manager/internal/clients/ado"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/models"
)

// fakeStore is an in-memory CheckpointStore. It lets the orchestrator's
// idempotency/resumability logic be exercised without a database.
type fakeStore struct {
	mu        sync.Mutex
	migration *models.RepoMigration
	steps     map[stepKey]models.RepoMigrationStep
	upsertErr error
}

func newFakeStore(m *models.RepoMigration) *fakeStore {
	return &fakeStore{migration: m, steps: map[stepKey]models.RepoMigrationStep{}}
}

func (f *fakeStore) GetMigration(_ context.Context, id string) (*models.RepoMigration, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.migration == nil || f.migration.ID != id {
		return nil, nil
	}
	cp := *f.migration
	return &cp, nil
}

func (f *fakeStore) UpdateMigration(_ context.Context, m *models.RepoMigration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := *m
	f.migration = &cp
	return nil
}

func (f *fakeStore) ListSteps(_ context.Context, _ string) ([]models.RepoMigrationStep, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]models.RepoMigrationStep, 0, len(f.steps))
	for _, s := range f.steps {
		out = append(out, s)
	}
	return out, nil
}

func (f *fakeStore) UpsertStep(_ context.Context, s *models.RepoMigrationStep) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.upsertErr != nil {
		return f.upsertErr
	}
	f.steps[stepKey{s.ResourceType, s.ResourceKey}] = *s
	return nil
}

func (f *fakeStore) stepStatus(rt, rk string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.steps[stepKey{rt, rk}].Status
}

// fakeTarget records calls and can inject conflicts/errors per resource name.
type fakeTarget struct {
	conflictRepos map[string]bool
	failPipelines map[string]error
	// repoRemoteURL, when set, is returned as the created repository's RemoteURL
	// so a real GoGitPusher has a fixture target to push history into.
	repoRemoteURL string

	createdRepos  []string
	createdPipes  []string
	adoptedPols   int
	adoptedGroups []string
	adoptedConns  []string
	repoCounter   int
}

func (t *fakeTarget) CreateRepository(_ context.Context, name string) (*ado.Repository, error) {
	if t.conflictRepos[name] {
		return nil, &ado.APIError{StatusCode: 409, Body: "exists"}
	}
	t.createdRepos = append(t.createdRepos, name)
	t.repoCounter++
	return &ado.Repository{ID: name + "-id", Name: name, RemoteURL: t.repoRemoteURL}, nil
}

func (t *fakeTarget) CreatePipeline(_ context.Context, req ado.CreatePipelineRequest) (*ado.Pipeline, error) {
	if err, ok := t.failPipelines[req.Name]; ok {
		return nil, err
	}
	t.createdPipes = append(t.createdPipes, req.Name)
	return &ado.Pipeline{ID: len(t.createdPipes), Name: req.Name}, nil
}

func (t *fakeTarget) AdoptBranchPolicy(_ context.Context, req ado.AdoptBranchPolicyRequest) (*ado.BranchPolicy, error) {
	t.adoptedPols++
	return &ado.BranchPolicy{Type: req.TypeID}, nil
}

func (t *fakeTarget) AdoptVariableGroup(_ context.Context, name string, _ []string) (*ado.VariableGroup, error) {
	t.adoptedGroups = append(t.adoptedGroups, name)
	return &ado.VariableGroup{Name: name}, nil
}

func (t *fakeTarget) AdoptServiceConnection(_ context.Context, req ado.AdoptServiceConnectionRequest) (*ado.ServiceConnection, error) {
	t.adoptedConns = append(t.adoptedConns, req.Name)
	return &ado.ServiceConnection{Name: req.Name}, nil
}

// samplePlan returns a small plan covering all five resource types.
func samplePlan() *ado.MigrationPlan {
	return &ado.MigrationPlan{
		Repositories:       []ado.Repository{{ID: "src1", Name: "platform-infra"}},
		Pipelines:          []ado.Pipeline{{ID: 1, Name: "platform-infra-CI", Folder: "\\Infra"}},
		BranchPolicies:     []ado.BranchPolicy{{ID: 11, Type: "type-a", DisplayName: "Min reviewers", IsEnabled: true}},
		VariableGroups:     []ado.VariableGroup{{ID: 4, Name: "platform-shared", VariableNames: []string{"A", "B"}}},
		ServiceConnections: []ado.ServiceConnection{{ID: "c1", Name: "azure-prod", Type: "azurerm"}},
	}
}

func newMigration() *models.RepoMigration {
	return &models.RepoMigration{
		ID:            "mig-1",
		TargetProject: "ContosoTarget",
		Status:        models.RepoMigrationStatusPending,
	}
}

// TestExecute_AllCreated provisions every resource on a clean target.
func TestExecute_AllCreated(t *testing.T) {
	store := newFakeStore(newMigration())
	target := &fakeTarget{}
	svc := NewService(store, nil, nil)

	sum, err := svc.Execute(context.Background(), "mig-1", samplePlan(), target)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if sum.Status != models.RepoMigrationStatusCompleted {
		t.Errorf("status = %q, want completed", sum.Status)
	}
	if sum.Total != 5 {
		t.Errorf("total = %d, want 5", sum.Total)
	}
	if sum.Created != 5 || sum.Skipped != 0 || sum.Failed != 0 {
		t.Errorf("counts created=%d skipped=%d failed=%d, want 5/0/0", sum.Created, sum.Skipped, sum.Failed)
	}
	if len(target.createdRepos) != 1 || len(target.createdPipes) != 1 {
		t.Errorf("expected 1 repo + 1 pipeline created, got %d/%d", len(target.createdRepos), len(target.createdPipes))
	}
	if target.adoptedPols != 1 || len(target.adoptedGroups) != 1 || len(target.adoptedConns) != 1 {
		t.Errorf("adopt counts pol=%d grp=%d conn=%d, want 1/1/1", target.adoptedPols, len(target.adoptedGroups), len(target.adoptedConns))
	}
	// Pipeline must be linked to the created repo's id.
	if store.stepStatus(models.RepoMigrationResourcePipeline, "platform-infra-CI") != models.RepoMigrationStepCreated {
		t.Error("pipeline step not recorded as created")
	}
}

// TestExecute_ConflictIsIdempotentSkip verifies a 409 on repo creation is
// recorded as skipped-exists, not failed, keeping the run successful.
func TestExecute_ConflictIsIdempotentSkip(t *testing.T) {
	store := newFakeStore(newMigration())
	target := &fakeTarget{conflictRepos: map[string]bool{"platform-infra": true}}
	svc := NewService(store, nil, nil)

	sum, err := svc.Execute(context.Background(), "mig-1", samplePlan(), target)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	// The conflicting repo is recorded as an idempotent skip, NOT a failure.
	if store.stepStatus(models.RepoMigrationResourceRepository, "platform-infra") != models.RepoMigrationStepSkippedExists {
		t.Error("conflicting repo not recorded as skipped-exists")
	}
	if sum.Skipped != 1 {
		t.Errorf("skipped = %d, want 1 (conflicting repo)", sum.Skipped)
	}
	// With the repo skipped (no id captured), the pipeline has no backing repo
	// and must fail — exercising the "no target repository" guard — which makes
	// the overall run status failed.
	if sum.Failed != 1 {
		t.Errorf("failed = %d, want 1 (pipeline has no backing repo)", sum.Failed)
	}
	if sum.Status != models.RepoMigrationStatusFailed {
		t.Errorf("status = %q, want failed (pipeline could not be linked)", sum.Status)
	}
}

// TestExecute_ResumeSkipsCompletedSteps seeds a terminal checkpoint for the repo
// and verifies the orchestrator does not re-create it on a resumed run.
func TestExecute_ResumeSkipsCompletedSteps(t *testing.T) {
	store := newFakeStore(newMigration())
	// Pre-seed: repo + pipeline already created in a prior (interrupted) run.
	store.steps[stepKey{models.RepoMigrationResourceRepository, "platform-infra"}] = models.RepoMigrationStep{
		ResourceType: models.RepoMigrationResourceRepository,
		ResourceKey:  "platform-infra",
		Status:       models.RepoMigrationStepCreated,
	}
	store.steps[stepKey{models.RepoMigrationResourcePipeline, "platform-infra-CI"}] = models.RepoMigrationStep{
		ResourceType: models.RepoMigrationResourcePipeline,
		ResourceKey:  "platform-infra-CI",
		Status:       models.RepoMigrationStepCreated,
	}
	target := &fakeTarget{}
	svc := NewService(store, nil, nil)

	sum, err := svc.Execute(context.Background(), "mig-1", samplePlan(), target)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if sum.Status != models.RepoMigrationStatusCompleted {
		t.Errorf("status = %q, want completed", sum.Status)
	}
	// The repo and pipeline must NOT be recreated (resumed).
	if len(target.createdRepos) != 0 {
		t.Errorf("repo recreated on resume: %v", target.createdRepos)
	}
	if len(target.createdPipes) != 0 {
		t.Errorf("pipeline recreated on resume: %v", target.createdPipes)
	}
	// The remaining three resources are still provisioned.
	if target.adoptedPols != 1 || len(target.adoptedGroups) != 1 || len(target.adoptedConns) != 1 {
		t.Errorf("remaining resources not provisioned: pol=%d grp=%d conn=%d", target.adoptedPols, len(target.adoptedGroups), len(target.adoptedConns))
	}
	// 2 resumed (skipped) + 3 created.
	if sum.Created != 3 || sum.Skipped != 2 {
		t.Errorf("counts created=%d skipped=%d, want 3/2", sum.Created, sum.Skipped)
	}
}

// TestExecute_PerResourceFailureDoesNotAbort injects a non-conflict pipeline
// error and verifies the remaining resources still run and the run ends failed.
func TestExecute_PerResourceFailureDoesNotAbort(t *testing.T) {
	store := newFakeStore(newMigration())
	target := &fakeTarget{failPipelines: map[string]error{"platform-infra-CI": errors.New("boom")}}
	svc := NewService(store, nil, nil)

	sum, err := svc.Execute(context.Background(), "mig-1", samplePlan(), target)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if sum.Status != models.RepoMigrationStatusFailed {
		t.Errorf("status = %q, want failed", sum.Status)
	}
	if sum.Failed != 1 {
		t.Errorf("failed = %d, want 1", sum.Failed)
	}
	// Repo (before) and the three adopts (after) still ran.
	if len(target.createdRepos) != 1 {
		t.Errorf("repo not created: %v", target.createdRepos)
	}
	if target.adoptedPols != 1 || len(target.adoptedGroups) != 1 || len(target.adoptedConns) != 1 {
		t.Error("resources after the failed pipeline did not run")
	}
	if store.stepStatus(models.RepoMigrationResourcePipeline, "platform-infra-CI") != models.RepoMigrationStepFailed {
		t.Error("failed pipeline not recorded as failed")
	}
}

// TestExecute_MigrationNotFound verifies a missing run id is a hard error.
func TestExecute_MigrationNotFound(t *testing.T) {
	store := newFakeStore(newMigration())
	svc := NewService(store, nil, nil)
	if _, err := svc.Execute(context.Background(), "nope", samplePlan(), &fakeTarget{}); err == nil {
		t.Fatal("expected error for unknown migration id")
	}
}

// fakeGitPusher records invocations of the deferred git-history seam.
type fakeGitPusher struct{ calls int }

func (f *fakeGitPusher) Push(_ context.Context, _, _ ado.Repository) error {
	f.calls++
	return nil
}

// TestExecute_GitHistorySeamInvokedPerCreatedRepo verifies the deferred seam is
// called once per newly created repo and not for skipped ones.
func TestExecute_GitHistorySeamInvokedPerCreatedRepo(t *testing.T) {
	store := newFakeStore(newMigration())
	target := &fakeTarget{}
	pusher := &fakeGitPusher{}
	svc := NewService(store, pusher, nil)

	if _, err := svc.Execute(context.Background(), "mig-1", samplePlan(), target); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if pusher.calls != 1 {
		t.Errorf("git history pusher calls = %d, want 1 (one created repo)", pusher.calls)
	}
}
