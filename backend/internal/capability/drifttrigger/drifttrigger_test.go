package drifttrigger

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/terraform-state-manager/terraform-state-manager/internal/db/models"
	triggersvc "github.com/terraform-state-manager/terraform-state-manager/internal/services/drifttrigger"
)

// fakeEngine is a stand-in for the outbound-trigger service.
type fakeEngine struct {
	result *triggersvc.TriggerResult
	err    error
	last   triggersvc.TriggerRequest
	calls  int
}

func (f *fakeEngine) Trigger(_ context.Context, req triggersvc.TriggerRequest) (*triggersvc.TriggerResult, error) {
	f.calls++
	f.last = req
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
		Name:           "nightly-trigger",
		TaskType:       TaskType,
		Config:         raw,
	}
}

func TestExecute_Unconfigured_Skips(t *testing.T) {
	h := NewHandler(nil)
	if h.Configured() {
		t.Fatal("Configured() = true for nil engine, want false")
	}
	task := taskWithConfig(t, taskConfig{PipelineID: 7})
	if status := h.Execute(context.Background(), task); status != models.TaskRunStatusSkipped {
		t.Errorf("status = %q, want skipped when unconfigured", status)
	}
}

func TestExecute_MissingPipelineID_Fails(t *testing.T) {
	eng := &fakeEngine{result: &triggersvc.TriggerResult{}}
	h := NewHandler(eng)
	task := taskWithConfig(t, taskConfig{SourceID: "src-1"}) // PipelineID omitted
	if status := h.Execute(context.Background(), task); status != models.TaskRunStatusFailed {
		t.Errorf("status = %q, want failed for missing pipeline_id", status)
	}
	if eng.calls != 0 {
		t.Errorf("engine called %d times, want 0 for invalid config", eng.calls)
	}
}

func TestExecute_QueuesRun_Success(t *testing.T) {
	eng := &fakeEngine{result: &triggersvc.TriggerResult{RunID: 42, RunState: "inProgress"}}
	h := NewHandler(eng)
	task := taskWithConfig(t, taskConfig{
		SourceID:   "src-1",
		PipelineID: 7,
		Branch:     "main",
		Parameters: map[string]string{"env": "prod"},
	})
	if status := h.Execute(context.Background(), task); status != models.TaskRunStatusSuccess {
		t.Errorf("status = %q, want success", status)
	}
	if eng.calls != 1 {
		t.Fatalf("engine called %d times, want 1", eng.calls)
	}
	if eng.last.PipelineID != 7 || eng.last.SourceID != "src-1" || eng.last.Branch != "main" {
		t.Errorf("request = %+v, want pipeline 7 / src-1 / main", eng.last)
	}
}

func TestExecute_EngineError_Fails(t *testing.T) {
	eng := &fakeEngine{err: errors.New("ado unreachable")}
	h := NewHandler(eng)
	task := taskWithConfig(t, taskConfig{PipelineID: 7})
	if status := h.Execute(context.Background(), task); status != models.TaskRunStatusFailed {
		t.Errorf("status = %q, want failed on engine error", status)
	}
}

func TestFire_Unconfigured_Errors(t *testing.T) {
	h := NewHandler(nil)
	if _, err := h.Fire(context.Background(), triggersvc.TriggerRequest{PipelineID: 1}); err == nil {
		t.Error("Fire on unconfigured handler should error")
	}
}

func TestNew_CapabilityShape(t *testing.T) {
	cap := New(&fakeEngine{})
	if cap.Key != "drifttrigger" {
		t.Errorf("Key = %q, want drifttrigger", cap.Key)
	}
	if cap.TaskType != TaskType {
		t.Errorf("TaskType = %q, want %q", cap.TaskType, TaskType)
	}
	if cap.TaskHandler == nil {
		t.Error("TaskHandler must be set")
	}
	if len(cap.Scopes) != 0 {
		t.Errorf("Scopes = %v, want none (reuses drift:write)", cap.Scopes)
	}
}
