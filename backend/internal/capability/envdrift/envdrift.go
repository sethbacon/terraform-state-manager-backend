// Package envdrift implements the environment-drift capability — the worked
// example of the capability contract documented in backend/docs/capabilities.md.
//
// The capability answers a single question for a Terraform state: do its
// azurerm-managed resources still exist and still match in live Azure, or has
// the environment drifted out from under the state? It wraps the environment
// drift engine (internal/services/envdrift), which extracts the azurerm
// resources from a parsed state, looks each up via an Azure ResourceReader, and
// records a drift_events row (drift_source = "environment") when any resource is
// missing or changed.
//
// Unconfigured behaviour: live Azure access needs a credential
// (clients/cloud/azure.Credential) that is NOT provisioned yet — the deployment
// targets are placeholders. A capability constructed without a configured engine
// reports itself unavailable: its scheduled handler returns "skipped" (mirroring
// the backup task's nil-service skip) and its HTTP trigger returns 503, rather
// than panicking. Wire a real engine via New / NewHandler once a credential exists.
package envdrift

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/terraform-state-manager/terraform-state-manager/internal/capability"
	"github.com/terraform-state-manager/terraform-state-manager/internal/clients/hcp"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/models"
	envdriftsvc "github.com/terraform-state-manager/terraform-state-manager/internal/services/envdrift"
)

// TaskType is the scheduled_task.task_type handled by this capability. It mirrors
// models.TaskTypeEnvDrift, which ValidTaskTypes uses to accept API-created
// envdrift tasks.
const TaskType = models.TaskTypeEnvDrift

// Engine is the slice of the environment-drift service this capability needs.
// Depending on the interface (rather than the concrete *envdrift.Service) keeps
// the capability unit-testable with a fake engine and lets wiring code pass an
// untyped nil to signal "unconfigured" (a typed-nil concrete pointer would make
// Configured() incorrectly report true).
type Engine interface {
	DetectForState(
		ctx context.Context,
		orgID string,
		workspaceName string,
		state *hcp.StateFile,
		stateProps map[string]map[string]string,
	) (*envdriftsvc.Result, error)
}

// taskConfig is the per-task Config JSONB shape for an envdrift task. State is
// the parsed Terraform state to evaluate, supplied inline so the capability is
// runnable without a live state-fetch pipeline (deferred). WorkspaceName labels
// the drift event; it falls back to the task name when empty.
type taskConfig struct {
	WorkspaceName string         `json:"workspace_name,omitempty"`
	State         *hcp.StateFile `json:"state"`
}

// Handler runs environment-drift detection for a scheduled task. It is also the
// entry point the HTTP trigger calls. A nil engine marks the capability
// unconfigured (no Azure credential): scheduled runs skip and HTTP triggers 503.
type Handler struct {
	engine Engine
	logger *slog.Logger
}

// NewHandler constructs an envdrift Handler around an Engine. Pass nil for the
// engine to construct an unconfigured handler (Configured() reports false).
func NewHandler(engine Engine) *Handler {
	return &Handler{
		engine: engine,
		logger: slog.With("capability", "envdrift"),
	}
}

// New returns the capability registration record for the environment-drift
// capability, wiring the supplied engine. A nil engine yields an unconfigured
// capability that still registers (so the route and task type exist and are
// UAT-able) but skips/503s at runtime.
func New(engine Engine) capability.Capability {
	h := NewHandler(engine)
	return capability.Capability{
		Name:        "Environment Drift",
		Key:         "envdrift",
		TaskType:    TaskType,
		TaskHandler: h.Execute,
		// Reuses the existing drift:write scope (auth/scopes.go); no new scope.
		Scopes: nil,
	}
}

// Configured reports whether a live drift engine is wired. When false the engine
// has no Azure credential and the capability is unavailable.
func (h *Handler) Configured() bool { return h.engine != nil }

// Detect runs environment-drift detection for the supplied parsed state. It is
// the shared entry point for both the scheduled handler and the HTTP trigger.
// The caller must check Configured() first; Detect returns an error if the
// engine is nil.
func (h *Handler) Detect(
	ctx context.Context,
	orgID, workspaceName string,
	state *hcp.StateFile,
) (*envdriftsvc.Result, error) {
	if h.engine == nil {
		return nil, fmt.Errorf("envdrift: engine not configured (no Azure credential)")
	}
	// stateProps is nil: an existence-only comparison. Per-property change
	// detection from recorded state attributes is a later refinement.
	return h.engine.DetectForState(ctx, orgID, workspaceName, state, nil)
}

// Execute is the capability.TaskHandler. It returns a models.TaskRunStatus*
// constant. When the capability is unconfigured it returns "skipped"; a
// successful detection (drift or no drift) returns "success"; a config or engine
// error returns "failed".
func (h *Handler) Execute(ctx context.Context, task *models.ScheduledTask) string {
	logger := h.logger.With("task_id", task.ID, "org_id", task.OrganizationID)

	if !h.Configured() {
		logger.Warn("Environment-drift engine not configured (no Azure credential); skipping task")
		return models.TaskRunStatusSkipped
	}

	cfg, err := parseConfig(task.Config)
	if err != nil {
		logger.Error("invalid envdrift task config", "error", err)
		return models.TaskRunStatusFailed
	}

	workspace := cfg.WorkspaceName
	if workspace == "" {
		workspace = task.Name
	}

	result, err := h.Detect(ctx, task.OrganizationID, workspace, cfg.State)
	if err != nil {
		logger.Error("environment-drift detection failed", "error", err, "workspace", workspace)
		return models.TaskRunStatusFailed
	}

	if result.DriftEventID == "" {
		logger.Info("no environment drift detected", "workspace", workspace)
	} else {
		logger.Info("environment drift detected",
			"workspace", workspace,
			"severity", result.Severity,
			"drift_event_id", result.DriftEventID)
	}
	return models.TaskRunStatusSuccess
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
	if cfg.State == nil {
		return nil, fmt.Errorf("state is required (live state fetch is deferred)")
	}
	return &cfg, nil
}
