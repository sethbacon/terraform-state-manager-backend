package versiontest

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"github.com/terraform-state-manager/terraform-state-manager/internal/db/models"
)

// fakeSink captures drift events written by the handler.
type fakeSink struct {
	events []*models.DriftEvent
	err    error
}

func (f *fakeSink) Create(_ context.Context, event *models.DriftEvent) error {
	if f.err != nil {
		return f.err
	}
	f.events = append(f.events, event)
	return nil
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
		Name:           "nightly-version-test",
		TaskType:       TaskType,
		Config:         raw,
	}
}

func TestExecute_NoOp_WritesNoEvent(t *testing.T) {
	sink := &fakeSink{}
	h := NewHandler(NewFixturePlanProvider(), sink)

	task := taskWithConfig(t, taskConfig{
		RepoURL:           "https://example/repo",
		CandidateVersions: []string{"5.40.0"},
		PlanFixture:       filepath.Join("testdata", "plan_no_op.json"),
	})

	status := h.Execute(context.Background(), task)
	if status != models.TaskRunStatusSuccess {
		t.Fatalf("status = %q, want success", status)
	}
	if len(sink.events) != 0 {
		t.Errorf("no-op should write no drift events, got %d", len(sink.events))
	}
}

func TestExecute_Drift_WritesEvent(t *testing.T) {
	sink := &fakeSink{}
	h := NewHandler(NewFixturePlanProvider(), sink)

	task := taskWithConfig(t, taskConfig{
		RepoURL:           "https://example/repo",
		CandidateVersions: []string{"5.40.0"},
		PlanFixture:       filepath.Join("testdata", "plan_drift.json"),
	})

	status := h.Execute(context.Background(), task)
	if status != models.TaskRunStatusSuccess {
		t.Fatalf("status = %q, want success", status)
	}
	if len(sink.events) != 1 {
		t.Fatalf("drift should write one event, got %d", len(sink.events))
	}

	ev := sink.events[0]
	if ev.OrganizationID != "org-1" {
		t.Errorf("event org = %q, want org-1", ev.OrganizationID)
	}
	if ev.WorkspaceName != "nightly-version-test" {
		t.Errorf("event workspace = %q, want task name", ev.WorkspaceName)
	}
	if ev.DriftSource != models.DriftSourceCode {
		t.Errorf("event drift_source = %q, want code", ev.DriftSource)
	}
	// One in-place update => warning severity, modified count 1.
	if ev.Severity != models.DriftSeverityWarning {
		t.Errorf("event severity = %q, want warning", ev.Severity)
	}
	if ev.ExternalRef == nil || *ev.ExternalRef != "versiontest:https://example/repo@5.40.0" {
		t.Errorf("event external_ref = %v, want versiontest:https://example/repo@5.40.0", ev.ExternalRef)
	}

	var changes struct {
		Modified []string `json:"modified"`
	}
	if err := json.Unmarshal(ev.Changes, &changes); err != nil {
		t.Fatalf("unmarshal changes: %v", err)
	}
	if len(changes.Modified) != 1 || changes.Modified[0] != "aws_s3_bucket.logs" {
		t.Errorf("changes.modified = %v, want [aws_s3_bucket.logs]", changes.Modified)
	}
}

func TestExecute_MissingFixture_Fails(t *testing.T) {
	sink := &fakeSink{}
	h := NewHandler(NewFixturePlanProvider(), sink)

	task := taskWithConfig(t, taskConfig{
		RepoURL:     "https://example/repo",
		PlanFixture: "", // required
	})

	if status := h.Execute(context.Background(), task); status != models.TaskRunStatusFailed {
		t.Errorf("status = %q, want failed for missing plan_fixture", status)
	}
}

func TestExecute_ProviderError_Fails(t *testing.T) {
	sink := &fakeSink{}
	h := NewHandler(NewFixturePlanProvider(), sink)

	task := taskWithConfig(t, taskConfig{
		RepoURL:     "https://example/repo",
		PlanFixture: filepath.Join("testdata", "does_not_exist.json"),
	})

	if status := h.Execute(context.Background(), task); status != models.TaskRunStatusFailed {
		t.Errorf("status = %q, want failed when fixture is unreadable", status)
	}
}

func TestExecute_SinkError_Fails(t *testing.T) {
	sink := &fakeSink{err: errors.New("db down")}
	h := NewHandler(NewFixturePlanProvider(), sink)

	task := taskWithConfig(t, taskConfig{
		RepoURL:     "https://example/repo",
		PlanFixture: filepath.Join("testdata", "plan_drift.json"),
	})

	if status := h.Execute(context.Background(), task); status != models.TaskRunStatusFailed {
		t.Errorf("status = %q, want failed when sink errors", status)
	}
}

func TestNew_CapabilityShape(t *testing.T) {
	cap := New(NewFixturePlanProvider(), &fakeSink{})
	if cap.Key != "versiontest" {
		t.Errorf("Key = %q, want versiontest", cap.Key)
	}
	if cap.TaskType != TaskType {
		t.Errorf("TaskType = %q, want %q", cap.TaskType, TaskType)
	}
	if cap.TaskHandler == nil {
		t.Error("TaskHandler must be set")
	}
	if len(cap.Scopes) != 1 || cap.Scopes[0] != ScopeAdmin {
		t.Errorf("Scopes = %v, want [%s]", cap.Scopes, ScopeAdmin)
	}
}
