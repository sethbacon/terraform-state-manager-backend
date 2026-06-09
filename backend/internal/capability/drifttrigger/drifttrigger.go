// Package drifttrigger implements the outbound drift-trigger capability: it
// queues a Terraform "plan" pipeline run in Azure DevOps on a schedule (or via
// the manual HTTP trigger). The plan it kicks off reports its results back to TSM
// through the inbound drift-ingest endpoint, closing the code-drift loop.
//
// It wraps the outbound-trigger engine (internal/services/drifttrigger), which
// acquires an Azure DevOps bearer via Workload Identity Federation and queues the
// run through the ADO REST client.
//
// Unconfigured behaviour: the ADO organization and the WIF token endpoint are
// placeholders until AKS + the Entra tenant exist. A capability constructed with
// a nil engine reports itself unavailable: its scheduled handler returns
// "skipped" and its HTTP trigger returns 503, mirroring the inbound drift-ingest
// 503-when-unconfigured behaviour, rather than panicking. Wire a real engine via
// New(svc) once the ADO/WIF configuration is present.
package drifttrigger

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/terraform-state-manager/terraform-state-manager/internal/capability"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/models"
	triggersvc "github.com/terraform-state-manager/terraform-state-manager/internal/services/drifttrigger"
)

// TaskType is the scheduled_task.task_type handled by this capability. It mirrors
// models.TaskTypeDriftTrigger, which ValidTaskTypes uses to accept API-created
// drifttrigger tasks.
const TaskType = models.TaskTypeDriftTrigger

// Engine is the slice of the outbound-trigger service this capability needs.
// Depending on the interface keeps the capability unit-testable with a fake and
// lets wiring code pass an untyped nil to signal "unconfigured" (a typed-nil
// concrete pointer would make Configured() incorrectly report true).
type Engine interface {
	Trigger(ctx context.Context, req triggersvc.TriggerRequest) (*triggersvc.TriggerResult, error)
}

// taskConfig is the per-task Config JSONB shape for a drifttrigger task. It maps
// directly onto a triggersvc.TriggerRequest.
type taskConfig struct {
	SourceID   string            `json:"source_id,omitempty"`
	PipelineID int               `json:"pipeline_id"`
	Branch     string            `json:"branch,omitempty"`
	Parameters map[string]string `json:"parameters,omitempty"`
}

// Handler queues outbound plan-pipeline runs for a scheduled task and is the
// entry point the HTTP trigger calls. A nil engine marks the capability
// unconfigured (no ADO/WIF): scheduled runs skip and HTTP triggers 503.
type Handler struct {
	engine Engine
	logger *slog.Logger
}

// NewHandler constructs a drifttrigger Handler around an engine. Pass nil for the
// engine to construct an unconfigured handler (Configured() reports false).
func NewHandler(engine Engine) *Handler {
	return &Handler{
		engine: engine,
		logger: slog.With("capability", "drifttrigger"),
	}
}

// New returns the capability registration record for the outbound drift-trigger
// capability, wiring the supplied engine. A nil engine yields an unconfigured
// capability that still registers (so the route and task type exist and are
// UAT-able) but skips/503s at runtime.
func New(engine Engine) capability.Capability {
	h := NewHandler(engine)
	return capability.Capability{
		Name:        "Outbound Drift Trigger",
		Key:         "drifttrigger",
		TaskType:    TaskType,
		TaskHandler: h.Execute,
		// Reuses the existing drift:write scope (auth/scopes.go); no new scope.
		Scopes: nil,
	}
}

// Configured reports whether a live trigger engine is wired. When false the ADO
// organization / WIF token endpoint are unset and the capability is unavailable.
func (h *Handler) Configured() bool { return h.engine != nil }

// Fire queues a single outbound plan-pipeline run. It is the shared entry point
// for both the scheduled handler and the HTTP trigger. The caller must check
// Configured() first; Fire returns an error if the engine is nil.
func (h *Handler) Fire(
	ctx context.Context,
	req triggersvc.TriggerRequest,
) (*triggersvc.TriggerResult, error) {
	if h.engine == nil {
		return nil, fmt.Errorf("drifttrigger: engine not configured (ADO/WIF unset)")
	}
	return h.engine.Trigger(ctx, req)
}

// Execute is the capability.TaskHandler. It returns a models.TaskRunStatus*
// constant. When the capability is unconfigured it returns "skipped"; a
// successfully queued run returns "success"; a config or engine error returns
// "failed".
func (h *Handler) Execute(ctx context.Context, task *models.ScheduledTask) string {
	logger := h.logger.With("task_id", task.ID, "org_id", task.OrganizationID)

	if !h.Configured() {
		logger.Warn("Outbound drift-trigger engine not configured (ADO/WIF unset); skipping task")
		return models.TaskRunStatusSkipped
	}

	cfg, err := parseConfig(task.Config)
	if err != nil {
		logger.Error("invalid drifttrigger task config", "error", err)
		return models.TaskRunStatusFailed
	}

	result, err := h.Fire(ctx, triggersvc.TriggerRequest{
		SourceID:   cfg.SourceID,
		PipelineID: cfg.PipelineID,
		Branch:     cfg.Branch,
		Parameters: cfg.Parameters,
	})
	if err != nil {
		logger.Error("outbound plan trigger failed", "error", err, "pipeline_id", cfg.PipelineID)
		return models.TaskRunStatusFailed
	}

	logger.Info("queued outbound plan pipeline run",
		"pipeline_id", cfg.PipelineID,
		"run_id", result.RunID,
		"run_state", result.RunState)
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
	if cfg.PipelineID <= 0 {
		return nil, fmt.Errorf("pipeline_id is required and must be positive")
	}
	return &cfg, nil
}
