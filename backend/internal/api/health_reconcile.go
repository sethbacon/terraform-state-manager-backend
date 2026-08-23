package api

import "github.com/terraform-state-manager/terraform-state-manager/internal/services/healthreconcile"

// healthFailureNotifier adapts the health handlers' notification path for the
// background reconciler: when a dispatched run is expired (no callback within the
// TTL), it fires the same run_failed alert a real failure callback would have
// produced, reusing notifyHealthResult (nil-notifier safe, detached send).
type healthFailureNotifier struct{ health *HealthHandlers }

// NotifyRunFailed implements healthreconcile.FailureNotifier. An expired run did
// not succeed, so it is reported as a failed status with success=false.
func (n healthFailureNotifier) NotifyRunFailed(organizationID, runID, detail string) {
	n.health.notifyHealthResult(organizationID, runID, "failed", false, detail)
}

var _ healthreconcile.FailureNotifier = healthFailureNotifier{}
