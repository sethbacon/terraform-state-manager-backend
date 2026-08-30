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

// Runner polls for due schedules on an interval and fires each via the Dispatcher.
type Runner struct {
	repo       *repositories.ScheduleRepository
	dispatcher Dispatcher
	interval   time.Duration
	stopCh     chan struct{}
	logger     *slog.Logger
}

// New constructs a Runner. Call Start to begin the background loop.
func New(repo *repositories.ScheduleRepository, dispatcher Dispatcher) *Runner {
	return &Runner{
		repo:       repo,
		dispatcher: dispatcher,
		interval:   defaultInterval,
		stopCh:     make(chan struct{}),
		logger:     slog.With("component", "scheduler"),
	}
}

// Start launches the polling loop in a goroutine and returns immediately.
func (r *Runner) Start() {
	ticker := time.NewTicker(r.interval)
	telemetry.RegisterWorker("scheduler", r.interval)
	r.logger.Info("scheduler started", "interval", r.interval.String())
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
	due, err := r.repo.GetDue(ctx, time.Now())
	cancel()
	if err != nil {
		r.logger.Error("failed to query due schedules", "error", err)
		return
	}
	for i := range due {
		r.fire(&due[i])
	}
}

// fire claims one due schedule (atomically advancing next_run_at), dispatches
// it, and records the outcome. Claiming BEFORE dispatch makes a firing
// at-most-once: a dispatch or outcome-write failure cannot leave the schedule
// due and re-fire a real CI run on the next poll; a lost claim means another
// poll or replica took this firing. A schedule that is overdue (e.g. the server
// was down) fires once and is rescheduled from now — there is no catch-up storm.
func (r *Runner) fire(s *repositories.Schedule) {
	logger := r.logger.With("schedule_id", s.ID, "name", s.Name, "target", s.TargetType)
	// Own budget per firing, independent of the poll and of other schedules.
	ctx, cancel := context.WithTimeout(context.Background(), fireTimeout)
	defer cancel()

	if s.NextRunAt == nil { // GetDue filters these out; defensive
		return
	}
	next := ComputeNextRun(s.CronExpr, time.Now())
	claimed, err := r.repo.ClaimDue(ctx, s.ID, *s.NextRunAt, time.Now(), next)
	if err != nil {
		logger.Error("failed to claim schedule", "error", err)
		return
	}
	if !claimed {
		logger.Info("schedule already claimed elsewhere; skipping")
		return
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
		return
	}

	runID, status, err := r.dispatcher.Dispatch(ctx, s.TargetType, s.TargetConfig, "scheduler", sysScope)
	if err != nil {
		logger.Error("schedule dispatch failed", "error", err)
		if status == "" {
			status = "failed"
		}
	} else {
		logger.Info("schedule fired", "run_id", runID, "status", status)
	}

	var runIDPtr *string
	if runID != "" {
		runIDPtr = &runID
	}
	if recErr := r.repo.RecordOutcome(ctx, s.ID, status, runIDPtr); recErr != nil {
		logger.Error("failed to record schedule outcome", "error", recErr)
	}
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
