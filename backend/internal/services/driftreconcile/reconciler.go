// Package driftreconcile runs a background loop that expires drift runs stuck in
// "dispatched". When TSM dispatches a drift run it inserts a drift_runs row in
// "dispatched" and hands the CI job a one-shot callback token to POST its result
// back. If that job dies before calling back — the build fails its init/plan step,
// the agent crashes, the pipeline is cancelled, the network drops — the run would
// otherwise sit dispatched forever: no drift record opens, no alert fires, and the
// callback token stays live indefinitely. This sweep is the durable backstop that
// closes that gap for every failure mode and every pipeline template.
//
// Like the scheduler and statesync, it is a leaf service: it depends on the drift
// repository and a FailureNotifier interface, never on the HTTP layer, so the api
// package can supply the concrete notifier without an import cycle. It is a
// periodic worker, so the router starts it only on the workers-enabled replica.
package driftreconcile

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
)

const (
	// reconcileTimeout bounds one sweep (the select plus per-run expiries).
	reconcileTimeout = 30 * time.Second
	// batchSize caps how many stuck runs one sweep expires; a larger backlog
	// drains over subsequent sweeps so a flood can't fan out unbounded alerts.
	batchSize = 100
)

// FailureNotifier fires the alert for a run the reconciler expired. It is
// implemented in the api layer over the existing drift-failure notification path
// (so an expiry produces the same run_failed alert a real failure callback
// would); kept as an interface here so this package does not import api.
type FailureNotifier interface {
	NotifyRunFailed(runID, detail string)
}

// Reconciler sweeps for expired dispatched drift runs on an interval.
type Reconciler struct {
	repo     *repositories.DriftRepository
	notifier FailureNotifier
	ttl      time.Duration
	interval time.Duration
	now      func() time.Time // injected so tests control the cutoff without sleeping
	stopCh   chan struct{}
	logger   *slog.Logger
}

// New constructs a Reconciler. ttl is how long a run may sit dispatched before it
// is failed; interval is how often the sweep runs. Call Start to begin the loop.
func New(repo *repositories.DriftRepository, notifier FailureNotifier, ttl, interval time.Duration) *Reconciler {
	return &Reconciler{
		repo:     repo,
		notifier: notifier,
		ttl:      ttl,
		interval: interval,
		now:      time.Now,
		stopCh:   make(chan struct{}),
		logger:   slog.With("component", "driftreconcile"),
	}
}

// Start launches the sweep loop in a goroutine and returns immediately. The first
// sweep runs right away so a fresh boot reaps anything already overdue.
func (r *Reconciler) Start() {
	ticker := time.NewTicker(r.interval)
	r.logger.Info("drift reconciler started", "interval", r.interval.String(), "ttl", r.ttl.String())
	go func() {
		r.reconcile()
		for {
			select {
			case <-ticker.C:
				r.reconcile()
			case <-r.stopCh:
				ticker.Stop()
				r.logger.Info("drift reconciler stopped")
				return
			}
		}
	}()
}

// Stop ends the loop. Safe to call once.
func (r *Reconciler) Stop() { close(r.stopCh) }

// reconcile runs one sweep under a bounded context.
func (r *Reconciler) reconcile() {
	ctx, cancel := context.WithTimeout(context.Background(), reconcileTimeout)
	defer cancel()
	r.reconcileOnce(ctx)
}

// reconcileOnce fails every dispatched run older than the TTL and fires its
// failure alert. Each expiry is a compare-and-clear gated on the run still being
// dispatched and still holding its token, so a callback that lands concurrently
// wins (the run keeps its real result and no spurious alert fires).
func (r *Reconciler) reconcileOnce(ctx context.Context) {
	cutoff := r.now().Add(-r.ttl)
	runs, err := r.repo.ListExpiredDispatched(ctx, cutoff, batchSize)
	if err != nil {
		r.logger.Error("failed to query expired drift runs", "error", err)
		return
	}
	if len(runs) == 0 {
		return
	}

	detail := fmt.Sprintf("expired: no callback within %s", r.ttl)
	expired := 0
	for i := range runs {
		run := &runs[i]
		ok, err := r.repo.ExpireDispatched(ctx, run.ID, run.CallbackToken, detail)
		if err != nil {
			r.logger.Error("failed to expire drift run", "run_id", run.ID, "error", err)
			continue
		}
		if !ok {
			// A real callback claimed the run between the select and now — leave
			// its result intact and do not alert.
			continue
		}
		expired++
		r.notifier.NotifyRunFailed(run.ID, detail)
	}
	if expired > 0 {
		r.logger.Info("expired stuck drift runs", "count", expired, "ttl", r.ttl.String())
	}
}
