package scheduler

import (
	"context"
	"log/slog"
	"testing"

	"github.com/terraform-state-manager/terraform-state-manager/internal/capability"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/models"
)

// testScheduler returns a Scheduler with only the fields executeCapabilityTask
// touches set (registry + logger), matching what New() guarantees.
func testScheduler(reg *capability.Registry) *Scheduler {
	return &Scheduler{registry: reg, logger: slog.Default()}
}

func TestExecuteCapabilityTask_DispatchesToCapability(t *testing.T) {
	reg := capability.NewRegistry()
	var gotTask *models.ScheduledTask
	reg.Register(capability.Capability{
		Name:     "Fake",
		Key:      "fake",
		TaskType: "fake",
		TaskHandler: func(_ context.Context, task *models.ScheduledTask) string {
			gotTask = task
			return models.TaskRunStatusSuccess
		},
	})

	s := testScheduler(reg)
	task := &models.ScheduledTask{ID: "t1", TaskType: "fake"}

	if status := s.executeCapabilityTask(context.Background(), task); status != models.TaskRunStatusSuccess {
		t.Errorf("status = %q, want success", status)
	}
	if gotTask == nil || gotTask.ID != "t1" {
		t.Error("capability handler did not receive the task")
	}
}

func TestExecuteCapabilityTask_NoRegistry_Fails(t *testing.T) {
	s := testScheduler(nil)
	task := &models.ScheduledTask{ID: "t1", TaskType: "unknown"}

	if status := s.executeCapabilityTask(context.Background(), task); status != models.TaskRunStatusFailed {
		t.Errorf("status = %q, want failed with no registry", status)
	}
}

func TestExecuteCapabilityTask_NoMatch_Fails(t *testing.T) {
	reg := capability.NewRegistry()
	s := testScheduler(reg)
	task := &models.ScheduledTask{ID: "t1", TaskType: "unhandled"}

	if status := s.executeCapabilityTask(context.Background(), task); status != models.TaskRunStatusFailed {
		t.Errorf("status = %q, want failed when no capability matches", status)
	}
}
