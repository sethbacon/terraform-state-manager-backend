// Package migration implements the storage migration service for moving
// Terraform state files between different storage backends.
package migration

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/terraform-state-manager/terraform-state-manager/internal/db/models"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
	"github.com/terraform-state-manager/terraform-state-manager/internal/storage"
)

// validBackends lists the accepted storage backend identifiers.
var validBackends = map[string]bool{
	"s3":    true,
	"azure": true,
	"gcs":   true,
	"local": true,
}

// StorageFactory is a function that creates a storage.Backend from a backend
// type string and raw JSON configuration. This allows the migration service
// to remain decoupled from the concrete storage implementations.
type StorageFactory func(backendType string, config json.RawMessage) (storage.Backend, error)

// Service provides storage migration operations including job creation,
// execution, validation, and dry-run simulation.
type Service struct {
	migrationRepo  *repositories.MigrationRepository
	storageFactory StorageFactory
	logger         *slog.Logger
}

// NewService creates a new migration Service.
func NewService(
	migrationRepo *repositories.MigrationRepository,
	storageFactory StorageFactory,
	logger *slog.Logger,
) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{
		migrationRepo:  migrationRepo,
		storageFactory: storageFactory,
		logger:         logger,
	}
}

// CreateJob validates and persists a new migration job.
func (s *Service) CreateJob(ctx context.Context, job *models.MigrationJob) error {
	if err := s.ValidateJob(ctx, job); err != nil {
		return fmt.Errorf("migration job validation failed: %w", err)
	}

	job.Status = models.MigrationStatusPending
	if err := s.migrationRepo.Create(ctx, job); err != nil {
		return fmt.Errorf("failed to create migration job: %w", err)
	}

	s.logger.Info("Migration job created",
		"job_id", job.ID,
		"name", job.Name,
		"source", job.SourceBackend,
		"target", job.TargetBackend,
		"dry_run", job.DryRun)

	return nil
}

// ExecuteJob loads the migration job from the database and runs the migration
// in a background goroutine. Progress is tracked in the database.
func (s *Service) ExecuteJob(ctx context.Context, jobID string) error {
	job, err := s.migrationRepo.GetByID(ctx, jobID)
	if err != nil {
		return fmt.Errorf("failed to load migration job: %w", err)
	}
	if job == nil {
		return fmt.Errorf("migration job not found: %s", jobID)
	}

	if job.Status != models.MigrationStatusPending {
		return fmt.Errorf("migration job %s is not in pending state (current: %s)", jobID, job.Status)
	}

	// Start the migration in a background goroutine.
	go s.executeMigration(context.Background(), job)

	return nil
}

// ValidateJob checks that the source and target backend types are supported and
// that the configuration JSON can be parsed.
func (s *Service) ValidateJob(ctx context.Context, job *models.MigrationJob) error {
	if job.Name == "" {
		return fmt.Errorf("job name is required")
	}

	if !validBackends[job.SourceBackend] {
		return fmt.Errorf("unsupported source backend: %s", job.SourceBackend)
	}
	if !validBackends[job.TargetBackend] {
		return fmt.Errorf("unsupported target backend: %s", job.TargetBackend)
	}

	if job.SourceBackend == job.TargetBackend {
		// Validate that configs are different by comparing raw JSON.
		if string(job.SourceConfig) == string(job.TargetConfig) {
			return fmt.Errorf("source and target configurations are identical")
		}
	}

	// Validate that configs are valid JSON.
	var sourceMap map[string]interface{}
	if err := json.Unmarshal(job.SourceConfig, &sourceMap); err != nil {
		return fmt.Errorf("invalid source_config JSON: %w", err)
	}
	var targetMap map[string]interface{}
	if err := json.Unmarshal(job.TargetConfig, &targetMap); err != nil {
		return fmt.Errorf("invalid target_config JSON: %w", err)
	}

	return nil
}

// DryRunResult represents the outcome of a dry-run migration simulation.
type DryRunResult struct {
	TotalFiles int      `json:"total_files"`
	Files      []string `json:"files"`
	SourceSize int64    `json:"source_size_bytes"`
}

// DryRun simulates a migration by listing the files that would be migrated
// from the source to the target backend without actually transferring data.
func (s *Service) DryRun(ctx context.Context, job *models.MigrationJob) (*DryRunResult, error) {
	if err := s.ValidateJob(ctx, job); err != nil {
		return nil, fmt.Errorf("dry-run validation failed: %w", err)
	}

	// Create source client.
	sourceClient, err := s.storageFactory(job.SourceBackend, job.SourceConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create source storage client: %w", err)
	}

	// List all files from source.
	files, err := sourceClient.List(ctx, "")
	if err != nil {
		return nil, fmt.Errorf("failed to list files from source: %w", err)
	}

	// Calculate total size.
	var totalSize int64
	for _, f := range files {
		data, err := sourceClient.Get(ctx, f)
		if err != nil {
			s.logger.Warn("Failed to get file size during dry-run",
				"file", f, "error", err)
			continue
		}
		totalSize += int64(len(data))
	}

	return &DryRunResult{
		TotalFiles: len(files),
		Files:      files,
		SourceSize: totalSize,
	}, nil
}

// CancelJob sets a running or pending migration job to cancelled status.
func (s *Service) CancelJob(ctx context.Context, jobID string) error {
	job, err := s.migrationRepo.GetByID(ctx, jobID)
	if err != nil {
		return fmt.Errorf("failed to load migration job: %w", err)
	}
	if job == nil {
		return fmt.Errorf("migration job not found: %s", jobID)
	}

	if job.Status != models.MigrationStatusPending && job.Status != models.MigrationStatusRunning {
		return fmt.Errorf("only pending or running jobs can be cancelled (current: %s)", job.Status)
	}

	now := time.Now()
	job.Status = models.MigrationStatusCancelled
	job.CompletedAt = &now
	if err := s.migrationRepo.Update(ctx, job); err != nil {
		return fmt.Errorf("failed to cancel migration job: %w", err)
	}

	s.logger.Info("Migration job cancelled", "job_id", jobID)
	return nil
}

// executeMigration performs the actual file-by-file migration from source to
// target. It updates progress counters in the database as it runs.
func (s *Service) executeMigration(ctx context.Context, job *models.MigrationJob) {
	logger := s.logger.With("job_id", job.ID, "name", job.Name)

	// Update status to running.
	now := time.Now()
	job.Status = models.MigrationStatusRunning
	job.StartedAt = &now
	if err := s.migrationRepo.Update(ctx, job); err != nil {
		logger.Error("Failed to update job to running", "error", err)
		return
	}

	// Create source and target clients.
	sourceClient, err := s.storageFactory(job.SourceBackend, job.SourceConfig)
	if err != nil {
		s.failJob(ctx, job, fmt.Sprintf("failed to create source client: %v", err))
		return
	}

	targetClient, err := s.storageFactory(job.TargetBackend, job.TargetConfig)
	if err != nil {
		s.failJob(ctx, job, fmt.Sprintf("failed to create target client: %v", err))
		return
	}

	// List files from source.
	files, err := sourceClient.List(ctx, "")
	if err != nil {
		s.failJob(ctx, job, fmt.Sprintf("failed to list source files: %v", err))
		return
	}

	job.TotalFiles = len(files)
	if err := s.migrationRepo.Update(ctx, job); err != nil {
		logger.Warn("Failed to update total_files", "error", err)
	}

	logger.Info("Starting migration", "total_files", len(files))

	var (
		migrated int
		failed   int
		skipped  int
		errLog   []map[string]string
	)

	for _, filePath := range files {
		// Check for cancellation.
		if ctx.Err() != nil {
			logger.Info("Migration cancelled during execution")
			break
		}

		// Check if the job was cancelled externally by re-reading the status.
		currentJob, _ := s.migrationRepo.GetByID(ctx, job.ID)
		if currentJob != nil && currentJob.Status == models.MigrationStatusCancelled {
			logger.Info("Migration cancelled externally")
			break
		}

		// Download from source.
		data, err := sourceClient.Get(ctx, filePath)
		if err != nil {
			failed++
			errLog = append(errLog, map[string]string{
				"file":  filePath,
				"error": fmt.Sprintf("failed to download: %v", err),
			})
			logger.Warn("Failed to download file", "file", filePath, "error", err)
			s.updateProgress(ctx, job.ID, migrated, failed)
			continue
		}

		// Check if file already exists in target.
		exists, err := targetClient.Exists(ctx, filePath)
		if err == nil && exists {
			skipped++
			s.updateProgress(ctx, job.ID, migrated, failed)
			continue
		}

		// Upload to target.
		if err := targetClient.Put(ctx, filePath, data); err != nil {
			failed++
			errLog = append(errLog, map[string]string{
				"file":  filePath,
				"error": fmt.Sprintf("failed to upload: %v", err),
			})
			logger.Warn("Failed to upload file", "file", filePath, "error", err)
			s.updateProgress(ctx, job.ID, migrated, failed)
			continue
		}

		migrated++
		s.updateProgress(ctx, job.ID, migrated, failed)
	}

	// Finalize the job.
	completedAt := time.Now()
	job.MigratedFiles = migrated
	job.FailedFiles = failed
	job.SkippedFiles = skipped
	job.CompletedAt = &completedAt

	if failed > 0 {
		job.Status = models.MigrationStatusFailed
	} else {
		job.Status = models.MigrationStatusCompleted
	}

	// Serialize error log.
	if len(errLog) > 0 {
		errLogJSON, _ := json.Marshal(errLog)
		job.ErrorLog = errLogJSON
	}

	if err := s.migrationRepo.Update(ctx, job); err != nil {
		logger.Error("Failed to finalize migration job", "error", err)
		return
	}

	logger.Info("Migration completed",
		"status", job.Status,
		"migrated", migrated,
		"failed", failed,
		"skipped", skipped,
		"duration", time.Since(now).String())
}

// updateProgress is a helper that updates the progress counters in the database.
func (s *Service) updateProgress(ctx context.Context, jobID string, migrated, failed int) {
	if err := s.migrationRepo.UpdateProgress(ctx, jobID, migrated, failed); err != nil {
		s.logger.Warn("Failed to update migration progress",
			"job_id", jobID, "error", err)
	}
}

// failJob marks a migration job as failed with an error message.
func (s *Service) failJob(ctx context.Context, job *models.MigrationJob, errMsg string) {
	now := time.Now()
	job.Status = models.MigrationStatusFailed
	job.CompletedAt = &now

	errLog, _ := json.Marshal([]map[string]string{{"error": errMsg}})
	job.ErrorLog = errLog

	if err := s.migrationRepo.Update(ctx, job); err != nil {
		s.logger.Error("Failed to mark migration job as failed",
			"job_id", job.ID, "error", err, "original_error", errMsg)
	} else {
		s.logger.Warn("Migration job failed", "job_id", job.ID, "reason", errMsg)
	}
}
