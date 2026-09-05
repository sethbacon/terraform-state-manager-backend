// Package scheduler runs a background loop that periodically dispatches due
// schedules (cron-driven recurring tasks). It is a leaf service: it depends on the
// schedule repository and a Dispatcher interface, never on the HTTP layer, so the
// api package can provide the concrete dispatcher without an import cycle.
package scheduler

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
	"github.com/terraform-state-manager/terraform-state-manager/internal/telemetry"
	"github.com/terraform-state-manager/terraform-state-manager/internal/tenancy"
)

// defaultInterval is how often the runner polls for due schedules.
const defaultInterval = 60 * time.Second

// defaultBatchLimit bounds how many due schedules one poll reads when Options
// does not specify one (mirrors config.SchedulerConfig's own default so a
// Runner built without a Config-derived Options behaves the same way).
const defaultBatchLimit = 50

// fireTimeout bounds one schedule's claim + dispatch + outcome write. Each
// firing gets its own budget so a slow dispatch cannot exhaust the batch.
const fireTimeout = 30 * time.Second

// Dispatcher executes a schedule's target. It is implemented in the api layer
// (which can dispatch drift runs); kept here as an interface so this package does
// not import api. runID is the id of the work item created (e.g. a drift run);
// status is "success" | "failed" | "skipped".
//
// The seam carries a tenancy.SystemScope rather than a bare organization id --
// the #393 background-authority decision (option B). The worker has no
// principal, so the authority for everything the dispatch loads is DERIVED from
// the schedule row being fired, and it travels as a real scope so every by-id
// load under the dispatch goes through the same InScope readers the request
// path uses. A chain that crosses organizations then fails closed instead of
// silently succeeding, and the SystemScope's provenance names the schedule that
// led there.
type Dispatcher interface {
	Dispatch(ctx context.Context, targetType string, targetConfig json.RawMessage, actor string, derived tenancy.SystemScope) (runID, status string, err error)
}

// Options configures a Runner's pacing (Phase 2 — fleet-scale drift). Every
// field is optional: Interval and BatchLimit fall back to a package default
// when zero, and a zero MaxInFlight means "no cap" — the same "an install that
// never touches this keeps today's behavior" contract as the Config fields
// these are normally built from (config.SchedulerConfig, config.DriftConfig).
type Options struct {
	// Interval is how often the runner polls for due schedules. Zero uses
	// defaultInterval.
	Interval time.Duration
	// BatchLimit bounds how many due schedules one poll reads (GetDue's LIMIT),
	// so a large due cohort drains over several polls instead of landing on the
	// agent pool as one herd. Zero uses defaultBatchLimit.
	BatchLimit int
	// MaxInFlight caps how many drift runs may be "dispatched" or "running" at
	// once before fire defers further firings to a later poll. Zero (the
	// default) means unlimited: InFlight, if set, is still sampled for the
	// tsm_drift_runs_in_flight gauge, but the cap never defers.
	MaxInFlight int
	// InFlight reports the current in-flight drift-run count (e.g.
	// DriftRepository.CountRunsIn over ["dispatched","running"]). May be nil,
	// in which case neither the gauge nor the cap check runs.
	InFlight func(ctx context.Context) (int, error)
}

// Runner polls for due schedules on an interval and fires each via the Dispatcher.
type Runner struct {
	repo        *repositories.ScheduleRepository
	dispatcher  Dispatcher
	interval    time.Duration
	batchLimit  int
	maxInFlight int
	inFlight    func(ctx context.Context) (int, error)
	stopCh      chan struct{}
	logger      *slog.Logger
}

// New constructs a Runner. Call Start to begin the background loop.
func New(repo *repositories.ScheduleRepository, dispatcher Dispatcher, opts Options) *Runner {
	interval := opts.Interval
	if interval <= 0 {
		interval = defaultInterval
	}
	batchLimit := opts.BatchLimit
	if batchLimit <= 0 {
		batchLimit = defaultBatchLimit
	}
	return &Runner{
		repo:        repo,
		dispatcher:  dispatcher,
		interval:    interval,
		batchLimit:  batchLimit,
		maxInFlight: opts.MaxInFlight,
		inFlight:    opts.InFlight,
		stopCh:      make(chan struct{}),
		logger:      slog.With("component", "scheduler"),
	}
}

// Start launches the polling loop in a goroutine and returns immediately.
func (r *Runner) Start() {
	ticker := time.NewTicker(r.interval)
	telemetry.RegisterWorker("scheduler", r.interval)
	r.logger.Info("scheduler started", "interval", r.interval.String(),
		"batch_limit", r.batchLimit, "max_in_flight", r.maxInFlight)
	go func() {
		r.checkDue() // immediate first check on startup
		for {
			select {
			case <-ticker.C:
				// Liveness tick lives here, not in checkDue, so it measures
				// "the loop is running" — a manual/test checkDue doesn't count.
				telemetry.WorkerTick("scheduler")
				r.checkDue()
			case <-r.stopCh:
				ticker.Stop()
				r.logger.Info("scheduler stopped")
				return
			}
		}
	}()
}

// Stop ends the polling loop and withdraws its liveness registration. Safe to
// call once.
func (r *Runner) Stop() {
	close(r.stopCh)
	telemetry.UnregisterWorker("scheduler")
}

func (r *Runner) checkDue() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	due, err := r.repo.GetDue(ctx, time.Now(), r.batchLimit)
	cancel()
	if err != nil {
		r.logger.Error("failed to query due schedules", "error", err)
		return
	}
	// unclaimed tallies this tick's due-but-not-claimed schedules -- deferred
	// under the in-flight cap, or a claim that genuinely failed -- and backs
	// tsm_scheduler_due_backlog. A schedule another replica/poll claimed first
	// does NOT count: it is handled, just not by this call.
	unclaimed := 0
	for i := range due {
		if !r.fire(&due[i]) {
			unclaimed++
		}
	}
	telemetry.SetSchedulerDueBacklog(unclaimed)
}

// fire claims one due schedule (atomically advancing next_run_at), dispatches
// it, and records the outcome. Claiming BEFORE dispatch makes a firing
// at-most-once: a dispatch or outcome-write failure cannot leave the schedule
// due and re-fire a real CI run on the next poll; a lost claim means another
// poll or replica took this firing. A schedule that is overdue (e.g. the server
// was down) fires once and is rescheduled from now — there is no catch-up storm.
//
// The in-flight cap (Phase 2 fleet-scale pacing) is checked BEFORE ClaimDue,
// and checked FRESH on every call rather than once per poll: a cohort
// dispatched earlier in the SAME poll raises the true in-flight count, so a
// stale snapshot would let an entire batch through together instead of
// draining it under the cap. A deferral returns without claiming, so
// next_run_at is untouched and the row is retried on a later poll — checking
// the cap AFTER the claim would still bound concurrency, but it would also
// burn the claim, forcing the schedule to wait a full cron cycle instead of
// the next poll.
//
// The return value says whether the row was claimed THIS call (by this
// replica) or is already claimed (by a concurrent one) — i.e. whether it is
// still due-and-unclaimed afterward. It backs tsm_scheduler_due_backlog;
// callers other than checkDue have no use for it.
func (r *Runner) fire(s *repositories.Schedule) (claimed bool) {
	logger := r.logger.With("schedule_id", s.ID, "name", s.Name, "target", s.TargetType)
	// Own budget per firing, independent of the poll and of other schedules.
	ctx, cancel := context.WithTimeout(context.Background(), fireTimeout)
	defer cancel()

	if s.NextRunAt == nil { // GetDue filters these out; defensive
		return true
	}

	if r.inFlight != nil {
		n, err := r.inFlight(ctx)
		if err != nil {
			logger.Error("failed to read in-flight drift run count; proceeding without the cap this firing", "error", err)
		} else {
			telemetry.SetDriftRunsInFlight(n)
			if r.maxInFlight > 0 && n >= r.maxInFlight {
				logger.Info("in-flight cap reached; deferring", "in_flight", n, "max_in_flight", r.maxInFlight)
				telemetry.DriftDispatchResult("deferred")
				return false
			}
		}
	}

	next := ComputeNextRun(s.CronExpr, time.Now())
	claimedRow, err := r.repo.ClaimDue(ctx, s.ID, *s.NextRunAt, time.Now(), next)
	if err != nil {
		logger.Error("failed to claim schedule", "error", err)
		telemetry.DriftDispatchResult("failed")
		return false
	}
	if !claimedRow {
		logger.Info("schedule already claimed elsewhere; skipping")
		// Not a backlog item: another replica/poll already owns this firing, so
		// no dispatch-result outcome is this replica's to report.
		return true
	}

	// THE WORKER HAS NO PRINCIPAL, so its authority is DERIVED from the
	// SCHEDULE row it is firing -- "system, acting in the schedule's
	// organization" (#393 option B). The organization is carried in memory
	// since GetDue selected it, because there is no edge from a run back to
	// its schedule to join along (#436); the derivation turns it into a real
	// scope so every load under the dispatch is an InScope read.
	//
	// A schedule with NO organization derives NO authority. That is only
	// possible on a database restored from a pre-000034 backup, and it is a
	// refusal rather than a skip: the failure is logged with the schedule's
	// coordinates and recorded as the firing's outcome, so an operator can
	// find the row -- a silent skip here would hide it forever. Derived AFTER
	// the claim, mirroring a dispatch failure, so the firing stays
	// at-most-once and surfaces in last_status instead of hot-looping.
	sysScope, err := tenancy.SystemActingIn(s.OrganizationID, "schedules", s.ID)
	if err != nil {
		logger.Error("schedule dispatch refused: no derivable authority", "error", err)
		if recErr := r.repo.RecordOutcome(ctx, s.ID, "failed", nil); recErr != nil {
			logger.Error("failed to record schedule outcome", "error", recErr)
		}
		telemetry.DriftDispatchResult("failed")
		return true
	}

	runID, status, err := r.dispatcher.Dispatch(ctx, s.TargetType, s.TargetConfig, "scheduler", sysScope)
	if err != nil {
		logger.Error("schedule dispatch failed", "error", err)
		if status == "" {
			status = "failed"
		}
		telemetry.DriftDispatchResult("failed")
	} else {
		logger.Info("schedule fired", "run_id", runID, "status", status)
		telemetry.DriftDispatchResult("ok")
	}

	var runIDPtr *string
	if runID != "" {
		runIDPtr = &runID
	}
	if recErr := r.repo.RecordOutcome(ctx, s.ID, status, runIDPtr); recErr != nil {
		logger.Error("failed to record schedule outcome", "error", recErr)
	}
	return true
}

// ComputeNextRun returns the next fire time for a schedule expression, or nil if
// it cannot be parsed (the caller treats nil as "no next run" — effectively
// pausing the schedule). It accepts a standard 5-field cron expression
// (e.g. "0 3 * * *") or the keywords "daily", "weekly", or "every <duration>"
// (e.g. "every 15m"; a minimum interval of 1 minute is enforced).
func ComputeNextRun(expr string, from time.Time) *time.Time {
	expr = strings.TrimSpace(expr)
	if sched, err := cron.ParseStandard(expr); err == nil {
		next := sched.Next(from)
		return &next
	}

	var next time.Time
	switch {
	case expr == "daily":
		next = from.Add(24 * time.Hour)
	case expr == "weekly":
		next = from.Add(7 * 24 * time.Hour)
	case strings.HasPrefix(expr, "every "):
		d, err := time.ParseDuration(strings.TrimSpace(strings.TrimPrefix(expr, "every ")))
		if err != nil {
			return nil
		}
		if d < time.Minute {
			d = time.Minute
		}
		next = from.Add(d)
	default:
		return nil
	}
	return &next
}
