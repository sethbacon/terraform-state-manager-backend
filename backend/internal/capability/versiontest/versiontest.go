package versiontest

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/terraform-state-manager/terraform-state-manager/internal/capability"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/models"
	ingestsvc "github.com/terraform-state-manager/terraform-state-manager/internal/services/driftingest"
)

// TaskType is the scheduled_task.task_type handled by this capability. It mirrors
// models.TaskTypeVersionTest, which ValidTaskTypes uses to accept API-created
// versiontest tasks.
const TaskType = models.TaskTypeVersionTest

// ScopeAdmin is the RBAC scope introduced by this capability.
const ScopeAdmin = "versiontest:admin"

// driftEventSink is the subset of the drift-event repository this capability
// needs. It is satisfied by *repositories.DriftEventRepository and kept narrow so
// the capability can be unit-tested with a fake.
type driftEventSink interface {
	Create(ctx context.Context, event *models.DriftEvent) error
}

// taskConfig is the per-task Config JSONB shape for a versiontest task.
type taskConfig struct {
	RepoURL           string   `json:"repo_url"`
	CandidateVersions []string `json:"candidate_versions"`
	PlanFixture       string   `json:"plan_fixture,omitempty"`
}

// Handler runs the version-no-op-test logic for a scheduled task. It obtains a
// plan from the provider, classifies it with the shared driftingest parser, and
// records a drift event when the candidate version is not a no-op.
type Handler struct {
	provider  PlanProvider
	driftRepo driftEventSink
	logger    *slog.Logger
}

// NewHandler constructs a versiontest Handler.
func NewHandler(provider PlanProvider, driftRepo driftEventSink) *Handler {
	return &Handler{
		provider:  provider,
		driftRepo: driftRepo,
		logger:    slog.With("capability", "versiontest"),
	}
}

// New returns the capability registration record for the version-no-op-test
// capability, wiring the supplied plan provider and drift-event sink.
func New(provider PlanProvider, driftRepo driftEventSink) capability.Capability {
	h := NewHandler(provider, driftRepo)
	return capability.Capability{
		Name:        "Version No-Op Test",
		Key:         "versiontest",
		TaskType:    TaskType,
		TaskHandler: h.Execute,
		Scopes:      []string{ScopeAdmin},
	}
}

// Execute is the capability.TaskHandler. It returns a models.TaskRunStatus*
// constant. A no-op candidate records no drift event and returns success; a
// drifting candidate writes a code-sourced drift event and returns success.
// Configuration or provider errors return failure.
func (h *Handler) Execute(ctx context.Context, task *models.ScheduledTask) string {
	logger := h.logger.With("task_id", task.ID, "org_id", task.OrganizationID)

	cfg, err := parseConfig(task.Config)
	if err != nil {
		logger.Error("invalid versiontest task config", "error", err)
		return models.TaskRunStatusFailed
	}

	// Pick the candidate version under test (first entry); the fixture provider
	// ignores it, but it is recorded on the drift event for traceability.
	candidate := ""
	if len(cfg.CandidateVersions) > 0 {
		candidate = cfg.CandidateVersions[0]
	}

	// The fixture provider reads its plan from the path on the context.
	planCtx := withFixturePath(ctx, cfg.PlanFixture)
	plan, err := h.provider.PlanFor(planCtx, cfg.RepoURL, candidate)
	if err != nil {
		logger.Error("failed to obtain plan", "error", err, "repo_url", cfg.RepoURL)
		return models.TaskRunStatusFailed
	}

	changes := ingestsvc.SummarizePlan(plan)

	// No-op: a clean version bump. Record nothing and report success.
	if !changes.HasChanges() {
		logger.Info("version candidate is a no-op",
			"repo_url", cfg.RepoURL, "candidate_version", candidate)
		return models.TaskRunStatusSuccess
	}

	// Drift: the candidate would change infrastructure. Record a code-sourced
	// drift event via the shared sink, mirroring the inbound drift-ingest path.
	if err := h.recordDrift(ctx, task, cfg, candidate, changes); err != nil {
		logger.Error("failed to record version drift", "error", err)
		return models.TaskRunStatusFailed
	}

	logger.Info("version candidate drifts",
		"repo_url", cfg.RepoURL,
		"candidate_version", candidate,
		"added", len(changes.Added),
		"removed", len(changes.Removed),
		"modified", len(changes.Modified))
	return models.TaskRunStatusSuccess
}

// recordDrift writes the drift summary to the drift_events sink as a
// code-sourced event, reusing the same JSONB shape and severity classifier as
// the inbound drift-ingest path.
func (h *Handler) recordDrift(
	ctx context.Context,
	task *models.ScheduledTask,
	cfg *taskConfig,
	candidate string,
	changes *ingestsvc.PlanChanges,
) error {
	changesBytes, err := json.Marshal(changes)
	if err != nil {
		return fmt.Errorf("marshal drift changes: %w", err)
	}

	severity := models.ClassifyDriftSeverity(
		len(changes.Added),
		len(changes.Removed),
		len(changes.Modified),
	)

	// external_ref makes the event traceable to the repo + candidate under test.
	externalRef := fmt.Sprintf("versiontest:%s@%s", cfg.RepoURL, candidate)

	event := &models.DriftEvent{
		OrganizationID: task.OrganizationID,
		WorkspaceName:  task.Name,
		Changes:        changesBytes,
		Severity:       severity,
		DriftSource:    models.DriftSourceCode,
		ExternalRef:    &externalRef,
	}
	return h.driftRepo.Create(ctx, event)
}

// parseConfig decodes the task Config JSONB into a taskConfig and validates the
// fields this capability requires.
func parseConfig(raw json.RawMessage) (*taskConfig, error) {
	var cfg taskConfig
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return nil, fmt.Errorf("decode config: %w", err)
		}
	}
	if cfg.PlanFixture == "" {
		return nil, fmt.Errorf("plan_fixture is required (live provider is deferred)")
	}
	return &cfg, nil
}
