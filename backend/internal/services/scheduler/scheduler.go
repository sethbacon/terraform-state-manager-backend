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
)

// defaultInterval is how often the runner polls for due schedules.
const defaultInterval = 60 * time.Second

// Dispatcher executes a schedule's target. It is implemented in the api layer
// (which can dispatch drift runs); kept here as an interface so this package does
// not import api. runID is the id of the work item created (e.g. a drift run);
// status is "success" | "failed" | "skipped".
type Dispatcher interface {
	Dispatch(ctx context.Context, targetType string, targetConfig json.RawMessage, actor string) (runID, status string, err error)
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

// Stop ends the polling loop. Safe to call once.
func (r *Runner) Stop() { close(r.stopCh) }

func (r *Runner) checkDue() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	due, err := r.repo.GetDue(ctx, time.Now())
	if err != nil {
		r.logger.Error("failed to query due schedules", "error", err)
		return
	}
	for i := range due {
		r.fire(ctx, &due[i])
	}
}

// fire dispatches one schedule and records the outcome + the next fire time. A
// schedule that is overdue (e.g. the server was down) fires once and is then
// rescheduled from now — there is no catch-up storm.
func (r *Runner) fire(ctx context.Context, s *repositories.Schedule) {
	logger := r.logger.With("schedule_id", s.ID, "name", s.Name, "target", s.TargetType)

	runID, status, err := r.dispatcher.Dispatch(ctx, s.TargetType, s.TargetConfig, "scheduler")
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
	next := ComputeNextRun(s.CronExpr, time.Now())
	if recErr := r.repo.RecordRun(ctx, s.ID, status, runIDPtr, time.Now(), next); recErr != nil {
		logger.Error("failed to record schedule run", "error", recErr)
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
