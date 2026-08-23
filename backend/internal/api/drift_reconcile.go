package api

import "github.com/terraform-state-manager/terraform-state-manager/internal/services/driftreconcile"

// driftFailureNotifier adapts the drift handlers' notification path for the
// background reconciler: when a dispatched run is expired (no callback within the
// TTL), it fires the same run_failed alert a real failure callback would have
// produced, reusing notifyDriftResult (nil-notifier safe, detached send).
type driftFailureNotifier struct{ drift *DriftHandlers }

// NotifyRunFailed implements driftreconcile.FailureNotifier.
func (n driftFailureNotifier) NotifyRunFailed(organizationID, runID, detail string) {
	n.drift.notifyDriftResult(organizationID, runID, "failed", 0, 0, 0, false, detail)
}

var _ driftreconcile.FailureNotifier = driftFailureNotifier{}
