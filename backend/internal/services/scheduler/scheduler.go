// Package scheduler implements a simple interval-based task scheduler that
// periodically checks for due scheduled tasks and dispatches them for execution.
package scheduler

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/robfig/cron/v3"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/models"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
	backupSvc "github.com/terraform-state-manager/terraform-state-manager/internal/services/backup"
	snapshotSvc "github.com/terraform-state-manager/terraform-state-manager/internal/services/snapshot"
)

const (
	// defaultCheckInterval is the frequency at which the scheduler polls for due tasks.
	defaultCheckInterval = 60 * time.Second
)

// Scheduler periodically checks for due scheduled tasks and executes them.
// It runs a background goroutine with a ticker that fires every 60 seconds.
type Scheduler struct {
	taskRepo    *repositories.ScheduledTaskRepository
	runRepo     *repositories.AnalysisRunRepository
	resultRepo  *repositories.AnalysisResultRepository
	sourceRepo  *repositories.StateSourceRepository
	snapshotSvc *snapshotSvc.Service
	backupSvc   *backupSvc.Service
	ticker      *time.Ticker
	stopCh      chan struct{}
	logger      *slog.Logger
}

// New creates a new Scheduler instance with the required repository and service dependencies.
func New(
	taskRepo *repositories.ScheduledTaskRepository,
	runRepo *repositories.AnalysisRunRepository,
	resultRepo *repositories.AnalysisResultRepository,
	sourceRepo *repositories.StateSourceRepository,
	snapshotService *snapshotSvc.Service,
	backupService *backupSvc.Service,
) *Scheduler {
	return &Scheduler{
		taskRepo:    taskRepo,
		runRepo:     runRepo,
		resultRepo:  resultRepo,
		sourceRepo:  sourceRepo,
		snapshotSvc: snapshotService,
		backupSvc:   backupService,
		stopCh:      make(chan struct{}),
		logger:      slog.With("component", "scheduler"),
	}
}

// Start begins the scheduler's background loop. It creates a ticker that fires
// every 60 seconds, checking for and executing any due tasks. Start returns
// immediately; the polling runs in a separate goroutine.
func (s *Scheduler) Start() {
	s.ticker = time.NewTicker(defaultCheckInterval)
	s.logger.Info("Scheduler started", "interval", defaultCheckInterval.String())

	go func() {
		// Run an initial check immediately on startup.
		s.checkDueTasks()

		for {
			select {
			case <-s.ticker.C:
				s.checkDueTasks()
			case <-s.stopCh:
				s.ticker.Stop()
				s.logger.Info("Scheduler stopped")
				return
			}
		}
	}()
}

// Stop gracefully stops the scheduler's background loop.
func (s *Scheduler) Stop() {
	close(s.stopCh)
}

// checkDueTasks queries the database for active tasks whose next_run_at is at or
// before now, then dispatches each one for execution.
func (s *Scheduler) checkDueTasks() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	now := time.Now()
	tasks, err := s.taskRepo.GetDueTasks(ctx, now)
	if err != nil {
		s.logger.Error("Failed to query due tasks", "error", err)
		return
	}

	if len(tasks) == 0 {
		return
	}

	s.logger.Info("Found due tasks", "count", len(tasks))

	for i := range tasks {
		s.executeTask(ctx, &tasks[i])
	}
}

// executeTask dispatches a single scheduled task based on its task_type.
// After execution it updates the task's last_run_at, last_run_status, and
// computes the next_run_at from the schedule string.
func (s *Scheduler) executeTask(ctx context.Context, task *models.ScheduledTask) {
	logger := s.logger.With("task_id", task.ID, "task_type", task.TaskType, "name", task.Name)
	logger.Info("Executing scheduled task")

	var status string

	switch task.TaskType {
	case models.TaskTypeAnalysis:
		status = s.executeAnalysisTask(ctx, task)
	case models.TaskTypeSnapshot:
		status = s.executeSnapshotTask(ctx, task)
	case models.TaskTypeReport:
		// Report generation is not yet implemented; mark as skipped.
		logger.Info("Report task type not yet implemented, skipping")
		status = models.TaskRunStatusSkipped
	case models.TaskTypeBackup:
		status = s.executeBackupTask(ctx, task)
	default:
		logger.Warn("Unknown task type", "task_type", task.TaskType)
		status = models.TaskRunStatusFailed
	}

	// Compute the next run time from the schedule string.
	nextRunAt := computeNextRun(task.Schedule, time.Now())

	ranAt := time.Now()
	if err := s.taskRepo.UpdateLastRun(ctx, task.ID, status, ranAt, nextRunAt); err != nil {
		logger.Error("Failed to update task after execution", "error", err)
	} else {
		logger.Info("Scheduled task execution completed", "status", status, "next_run_at", nextRunAt)
	}
}

// executeAnalysisTask creates a new analysis run for the task's organization.
func (s *Scheduler) executeAnalysisTask(ctx context.Context, task *models.ScheduledTask) string {
	logger := s.logger.With("task_id", task.ID)

	// Parse source_id from task config if present.
	var sourceID *string
	if task.Config != nil {
		var cfg map[string]interface{}
		if err := json.Unmarshal(task.Config, &cfg); err == nil {
			if sid, ok := cfg["source_id"].(string); ok && sid != "" {
				sourceID = &sid
			}
		}
	}

	runConfig, _ := json.Marshal(map[string]interface{}{
		"scheduled_task_id": task.ID,
	})

	run := &models.AnalysisRun{
		OrganizationID: task.OrganizationID,
		SourceID:       sourceID,
		Status:         models.RunStatusPending,
		TriggerType:    models.TriggerScheduled,
		Config:         runConfig,
		TriggeredBy:    task.CreatedBy,
	}

	if err := s.runRepo.Create(ctx, run); err != nil {
		logger.Error("Failed to create scheduled analysis run", "error", err)
		return models.TaskRunStatusFailed
	}

	logger.Info("Scheduled analysis run created", "run_id", run.ID)
	return models.TaskRunStatusSuccess
}

// executeSnapshotTask captures state snapshots from the latest completed analysis
// run for the task's source. It fetches all analysis results from that run and
// delegates to the snapshot service for persistence and drift detection.
func (s *Scheduler) executeSnapshotTask(ctx context.Context, task *models.ScheduledTask) string {
	logger := s.logger.With("task_id", task.ID)

	if s.snapshotSvc == nil {
		logger.Warn("Snapshot service not configured, skipping snapshot task")
		return models.TaskRunStatusSkipped
	}

	// Parse source_id from task config.
	var sourceID string
	if task.Config != nil {
		var cfg map[string]interface{}
		if err := json.Unmarshal(task.Config, &cfg); err == nil {
			if sid, ok := cfg["source_id"].(string); ok {
				sourceID = sid
			}
		}
	}

	if sourceID == "" {
		logger.Warn("No source_id in snapshot task config, skipping")
		return models.TaskRunStatusSkipped
	}

	// Find the latest completed analysis run for this source.
	latestRun, err := s.runRepo.GetLatestCompletedBySource(ctx, task.OrganizationID, sourceID)
	if err != nil {
		logger.Error("Failed to fetch latest completed run for snapshot", "error", err)
		return models.TaskRunStatusFailed
	}
	if latestRun == nil {
		logger.Info("No completed analysis run found for source, skipping snapshot", "source_id", sourceID)
		return models.TaskRunStatusSkipped
	}

	// Fetch all analysis results for that run.
	resultPtrs, err := s.resultRepo.GetAllByRunID(ctx, latestRun.ID)
	if err != nil {
		logger.Error("Failed to fetch analysis results for snapshot", "run_id", latestRun.ID, "error", err)
		return models.TaskRunStatusFailed
	}

	// Convert []*models.AnalysisResult to []models.AnalysisResult.
	results := make([]models.AnalysisResult, 0, len(resultPtrs))
	for _, r := range resultPtrs {
		results = append(results, *r)
	}

	snapshots, err := s.snapshotSvc.CaptureFromAnalysisResults(ctx, task.OrganizationID, &sourceID, results)
	if err != nil {
		logger.Error("Failed to capture snapshots from analysis results", "error", err)
		return models.TaskRunStatusFailed
	}

	logger.Info("Snapshot task completed", "snapshots_captured", len(snapshots), "source_id", sourceID)
	return models.TaskRunStatusSuccess
}

// executeBackupTask enforces retention policies for the task's organization.
// Full state backup creation requires access to raw state data which is outside
// the scheduler's scope; retention enforcement is the durable action taken here.
func (s *Scheduler) executeBackupTask(ctx context.Context, task *models.ScheduledTask) string {
	logger := s.logger.With("task_id", task.ID)

	if s.backupSvc == nil {
		logger.Warn("Backup service not configured, skipping backup task")
		return models.TaskRunStatusSkipped
	}

	if err := s.backupSvc.ApplyRetention(ctx, task.OrganizationID); err != nil {
		logger.Error("Failed to apply retention policy during backup task", "error", err)
		return models.TaskRunStatusFailed
	}

	logger.Info("Backup retention task completed", "org_id", task.OrganizationID)
	return models.TaskRunStatusSuccess
}

// computeNextRun calculates the next execution time based on a schedule string.
// It first attempts to parse the schedule as a standard cron expression
// (e.g. "0 * * * *"). If that fails it falls back to the simple keyword/duration
// formats:
//   - "every <N>m"  (minutes)
//   - "every <N>h"  (hours)
//   - "daily"       (24 hours from now)
//   - "weekly"      (168 hours from now)
//
// If the schedule cannot be parsed, it returns nil (no next run).
func computeNextRun(schedule string, from time.Time) *time.Time {
	// Try standard cron expression first.
	if cronSched, err := cron.ParseStandard(schedule); err == nil {
		next := cronSched.Next(from)
		return &next
	}

	var next time.Time

	switch {
	case schedule == "daily":
		next = from.Add(24 * time.Hour)
	case schedule == "weekly":
		next = from.Add(7 * 24 * time.Hour)
	case len(schedule) > 6 && schedule[:6] == "every ":
		durationStr := schedule[6:]
		d, err := time.ParseDuration(durationStr)
		if err != nil {
			return nil
		}
		if d < time.Minute {
			d = time.Minute // enforce a minimum interval of 1 minute
		}
		next = from.Add(d)
	default:
		// Unrecognized schedule format; do not set a next run.
		return nil
	}

	return &next
}
