package scheduler

import (
	"context"
	"testing"

	"github.com/terraform-state-manager/terraform-state-manager/internal/capability"
	capdrifttrigger "github.com/terraform-state-manager/terraform-state-manager/internal/capability/drifttrigger"
	capenvdrift "github.com/terraform-state-manager/terraform-state-manager/internal/capability/envdrift"
	"github.com/terraform-state-manager/terraform-state-manager/internal/capability/versiontest"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/models"
)

// TestDriftCapabilities_AreScheduled asserts the three capabilities wired in
// router.go are dispatchable through the scheduler's registry fallback, i.e. a
// scheduled_task of each capability's task type reaches its handler. This guards
// the "version-test (and the new drift capabilities) run on schedule" wiring.
//
// The envdrift and drifttrigger capabilities are constructed with nil engines
// (their default unconfigured state), so they return "skipped" — proving the
// dispatch path works without a live credential. versiontest with a missing
// fixture returns "failed", which is still proof the handler was invoked (a
// non-dispatched task would also be "failed", so versiontest is additionally
// covered by capability_dispatch_test.go's generic case and its own package).
func TestDriftCapabilities_AreScheduled(t *testing.T) {
	reg := capability.NewRegistry()
	reg.Register(versiontest.New(versiontest.NewFixturePlanProvider(), nil))
	reg.Register(capenvdrift.New(nil))
	reg.Register(capdrifttrigger.New(nil))

	s := testScheduler(reg)

	cases := []struct {
		taskType string
		want     string
	}{
		{models.TaskTypeEnvDrift, models.TaskRunStatusSkipped},
		{models.TaskTypeDriftTrigger, models.TaskRunStatusSkipped},
	}
	for _, tc := range cases {
		t.Run(tc.taskType, func(t *testing.T) {
			task := &models.ScheduledTask{ID: "t-" + tc.taskType, TaskType: tc.taskType}
			if got := s.executeCapabilityTask(context.Background(), task); got != tc.want {
				t.Errorf("task_type %q dispatched to status %q, want %q", tc.taskType, got, tc.want)
			}
		})
	}

	// versiontest must be looked up by its task type (proves it is registered and
	// schedulable); a registered-but-misconfigured task returns failed.
	if _, ok := reg.LookupByTaskType(models.TaskTypeVersionTest); !ok {
		t.Error("versiontest capability is not registered for scheduling")
	}
}
