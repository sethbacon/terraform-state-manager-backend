package envdrift

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/terraform-state-manager/terraform-state-manager/internal/clients/hcp"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/models"
	envdriftsvc "github.com/terraform-state-manager/terraform-state-manager/internal/services/envdrift"
)

// fakeEngine is a stand-in for the environment-drift service.
type fakeEngine struct {
	result *envdriftsvc.Result
	err    error
	calls  int
}

func (f *fakeEngine) DetectForState(
	_ context.Context,
	_ string,
	_ string,
	_ *hcp.StateFile,
	_ map[string]map[string]string,
) (*envdriftsvc.Result, error) {
	f.calls++
	return f.result, f.err
}

func taskWithConfig(t *testing.T, cfg taskConfig) *models.ScheduledTask {
	t.Helper()
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	return &models.ScheduledTask{
		ID:             "task-1",
		OrganizationID: "org-1",
		Name:           "nightly-envdrift",
		TaskType:       TaskType,
		Config:         raw,
	}
}

func sampleState() *hcp.StateFile {
	return &hcp.StateFile{Version: 4, Resources: []hcp.StateResource{}}
}

func TestExecute_Unconfigured_Skips(t *testing.T) {
	h := NewHandler(nil) // no engine => unconfigured
	if h.Configured() {
		t.Fatal("Configured() = true for nil engine, want false")
	}
	task := taskWithConfig(t, taskConfig{State: sampleState()})
	if status := h.Execute(context.Background(), task); status != models.TaskRunStatusSkipped {
		t.Errorf("status = %q, want skipped when unconfigured", status)
	}
}

func TestExecute_MissingState_Fails(t *testing.T) {
	eng := &fakeEngine{result: &envdriftsvc.Result{}}
	h := NewHandler(eng)
	// State omitted => parseConfig rejects.
	task := taskWithConfig(t, taskConfig{WorkspaceName: "ws"})
	if status := h.Execute(context.Background(), task); status != models.TaskRunStatusFailed {
		t.Errorf("status = %q, want failed for missing state", status)
	}
	if eng.calls != 0 {
		t.Errorf("engine called %d times, want 0 for invalid config", eng.calls)
	}
}

func TestExecute_NoDrift_Success(t *testing.T) {
	eng := &fakeEngine{result: &envdriftsvc.Result{Severity: "info"}}
	h := NewHandler(eng)
	task := taskWithConfig(t, taskConfig{State: sampleState()})
	if status := h.Execute(context.Background(), task); status != models.TaskRunStatusSuccess {
		t.Errorf("status = %q, want success", status)
	}
	if eng.calls != 1 {
		t.Errorf("engine called %d times, want 1", eng.calls)
	}
}

func TestExecute_DriftDetected_Success(t *testing.T) {
	eng := &fakeEngine{result: &envdriftsvc.Result{Severity: "critical", DriftEventID: "evt-1"}}
	h := NewHandler(eng)
	task := taskWithConfig(t, taskConfig{State: sampleState(), WorkspaceName: "prod"})
	if status := h.Execute(context.Background(), task); status != models.TaskRunStatusSuccess {
		t.Errorf("status = %q, want success", status)
	}
}

func TestExecute_EngineError_Fails(t *testing.T) {
	eng := &fakeEngine{err: errors.New("azure unreachable")}
	h := NewHandler(eng)
	task := taskWithConfig(t, taskConfig{State: sampleState()})
	if status := h.Execute(context.Background(), task); status != models.TaskRunStatusFailed {
		t.Errorf("status = %q, want failed on engine error", status)
	}
}

func TestDetect_Unconfigured_Errors(t *testing.T) {
	h := NewHandler(nil)
	if _, err := h.Detect(context.Background(), "org-1", "ws", sampleState()); err == nil {
		t.Error("Detect on unconfigured handler should error")
	}
}

func TestNew_CapabilityShape(t *testing.T) {
	cap := New(&fakeEngine{})
	if cap.Key != "envdrift" {
		t.Errorf("Key = %q, want envdrift", cap.Key)
	}
	if cap.TaskType != TaskType {
		t.Errorf("TaskType = %q, want %q", cap.TaskType, TaskType)
	}
	if cap.TaskHandler == nil {
		t.Error("TaskHandler must be set")
	}
	// Reuses the drift:write scope; introduces no new scope.
	if len(cap.Scopes) != 0 {
		t.Errorf("Scopes = %v, want none (reuses drift:write)", cap.Scopes)
	}
}
