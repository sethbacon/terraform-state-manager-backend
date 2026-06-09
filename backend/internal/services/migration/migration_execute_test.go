package migration

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/terraform-state-manager/terraform-state-manager/internal/db/models"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
	"github.com/terraform-state-manager/terraform-state-manager/internal/storage"
)

// migrationJobColumns mirrors the column order scanned by
// MigrationRepository.GetByID. Keep in sync with migration_repository.go.
var migrationJobColumns = []string{
	"id", "organization_id", "name", "source_backend", "source_config",
	"target_backend", "target_config", "status", "total_files",
	"migrated_files", "failed_files", "skipped_files", "error_log",
	"dry_run", "started_at", "completed_at", "created_by", "created_at", "updated_at",
}

// jobRow renders a single migration_jobs row for sqlmock from a model. It is
// used to satisfy the per-file cancellation re-read inside executeMigration.
func jobRow(job *models.MigrationJob) *sqlmock.Rows {
	return sqlmock.NewRows(migrationJobColumns).AddRow(
		job.ID, job.OrganizationID, job.Name, job.SourceBackend, job.SourceConfig,
		job.TargetBackend, job.TargetConfig, job.Status, job.TotalFiles,
		job.MigratedFiles, job.FailedFiles, job.SkippedFiles, job.ErrorLog,
		job.DryRun, job.StartedAt, job.CompletedAt, job.CreatedBy,
		time.Now(), time.Now(),
	)
}

// newExecService builds a migration Service whose repository is backed by a
// sqlmock database (no real Postgres) and whose storage factory is the real
// storage.NewBackendFromRawConfig — so jobs resolve independent backends from
// their per-job JSON config exactly as in production.
func newExecService(t *testing.T) (*Service, sqlmock.Sqlmock) {
	t.Helper()

	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	repo := repositories.NewMigrationRepository(db)
	svc := NewService(repo, storage.NewBackendFromRawConfig, nil)
	return svc, mock
}

// localConfig returns the raw JSON config the local backend factory expects.
func localConfig(t *testing.T, basePath string) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(map[string]string{"base_path": basePath})
	require.NoError(t, err)
	return raw
}

// expectExecuteFlow queues the sqlmock expectations issued by executeMigration:
// two leading Updates (status=running, then total_files), a GetByID +
// UpdateProgress for each of fileCount files, and a final Update. The job is
// reported as still "running" on each cancellation re-read so the loop proceeds.
func expectExecuteFlow(mock sqlmock.Sqlmock, job *models.MigrationJob, fileCount int) {
	// Status -> running, then total_files update.
	mock.ExpectExec("UPDATE migration_jobs").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("UPDATE migration_jobs").WillReturnResult(sqlmock.NewResult(0, 1))

	running := *job
	running.Status = models.MigrationStatusRunning
	for i := 0; i < fileCount; i++ {
		// Per-file cancellation re-read.
		mock.ExpectQuery("SELECT .* FROM migration_jobs").
			WithArgs(job.ID).
			WillReturnRows(jobRow(&running))
		// Per-file progress update.
		mock.ExpectExec("UPDATE migration_jobs").WillReturnResult(sqlmock.NewResult(0, 1))
	}

	// Final finalize update.
	mock.ExpectExec("UPDATE migration_jobs").WillReturnResult(sqlmock.NewResult(0, 1))
}

// TestExecuteMigration_LocalToLocal_EndToEnd drives the full executeMigration
// flow across two distinct local filesystem backends — resolved from per-job
// JSON config through the real storage factory. It asserts every source file
// lands byte-for-byte (checksum-verified) at the target and that the job
// finalizes as completed with the expected counters. This is the CI-safe
// "at least two backends" integration test (no external services).
func TestExecuteMigration_LocalToLocal_EndToEnd(t *testing.T) {
	ctx := context.Background()
	srcDir := t.TempDir()
	dstDir := t.TempDir()

	// Seed the source backend with several state files.
	files := map[string][]byte{
		"prod/terraform.tfstate":    []byte(`{"version":4,"serial":1,"env":"prod"}`),
		"staging/terraform.tfstate": []byte(`{"version":4,"serial":7,"env":"staging"}`),
		"dev/terraform.tfstate":     []byte(`{"version":4,"serial":3,"env":"dev"}`),
	}
	srcBackend, err := storage.NewBackendFromRawConfig("local", localConfig(t, srcDir))
	require.NoError(t, err)
	for path, content := range files {
		require.NoError(t, srcBackend.Put(ctx, path, content))
	}

	svc, mock := newExecService(t)

	job := &models.MigrationJob{
		ID:            "11111111-1111-1111-1111-111111111111",
		Name:          "local-to-local",
		SourceBackend: "local",
		SourceConfig:  localConfig(t, srcDir),
		TargetBackend: "local",
		TargetConfig:  localConfig(t, dstDir),
		Status:        models.MigrationStatusPending,
	}
	expectExecuteFlow(mock, job, len(files))

	svc.executeMigration(ctx, job)

	// The job must finalize as completed with all files migrated, none failed.
	assert.Equal(t, models.MigrationStatusCompleted, job.Status)
	assert.Equal(t, len(files), job.TotalFiles)
	assert.Equal(t, len(files), job.MigratedFiles)
	assert.Equal(t, 0, job.FailedFiles)
	assert.Equal(t, 0, job.SkippedFiles)
	require.NotNil(t, job.CompletedAt)

	// Every file must exist at the target with a byte-identical, checksum-matched
	// copy — verified by reading directly off the destination filesystem.
	dstBackend, err := storage.NewBackendFromRawConfig("local", localConfig(t, dstDir))
	require.NoError(t, err)
	for path, content := range files {
		got, getErr := dstBackend.Get(ctx, path)
		require.NoError(t, getErr, "target should hold %s", path)
		assert.Equal(t, content, got, "target bytes must match source for %s", path)
		assert.Equal(t, sha256Hex(content), sha256Hex(got),
			"target checksum must match source for %s", path)
	}

	require.NoError(t, mock.ExpectationsWereMet())
}

// TestExecuteMigration_LocalToLocal_Idempotent re-runs a migration against a
// target that already holds byte-identical copies and asserts every file is
// skipped (per #52), the source is left untouched, and the job still completes.
func TestExecuteMigration_LocalToLocal_Idempotent(t *testing.T) {
	ctx := context.Background()
	srcDir := t.TempDir()
	dstDir := t.TempDir()

	files := map[string][]byte{
		"a/terraform.tfstate": []byte(`{"version":4,"serial":1}`),
		"b/terraform.tfstate": []byte(`{"version":4,"serial":2}`),
	}
	srcBackend, err := storage.NewBackendFromRawConfig("local", localConfig(t, srcDir))
	require.NoError(t, err)
	dstBackend, err := storage.NewBackendFromRawConfig("local", localConfig(t, dstDir))
	require.NoError(t, err)
	for path, content := range files {
		require.NoError(t, srcBackend.Put(ctx, path, content))
		// Pre-seed the target with byte-identical content.
		require.NoError(t, dstBackend.Put(ctx, path, content))
	}

	svc, mock := newExecService(t)

	job := &models.MigrationJob{
		ID:            "22222222-2222-2222-2222-222222222222",
		Name:          "idempotent-rerun",
		SourceBackend: "local",
		SourceConfig:  localConfig(t, srcDir),
		TargetBackend: "local",
		TargetConfig:  localConfig(t, dstDir),
		Status:        models.MigrationStatusPending,
	}
	expectExecuteFlow(mock, job, len(files))

	svc.executeMigration(ctx, job)

	// A byte-identical target is a pure no-op skip: nothing migrated, nothing failed.
	assert.Equal(t, models.MigrationStatusCompleted, job.Status)
	assert.Equal(t, len(files), job.SkippedFiles)
	assert.Equal(t, 0, job.MigratedFiles)
	assert.Equal(t, 0, job.FailedFiles)

	require.NoError(t, mock.ExpectationsWereMet())
}

// TestExecuteMigration_LocalToLocal_DivergentTargetReTransferred runs a
// migration against a target that holds stale, divergent content and asserts
// the target is re-transferred so it converges on the source (per #52).
func TestExecuteMigration_LocalToLocal_DivergentTargetReTransferred(t *testing.T) {
	ctx := context.Background()
	srcDir := t.TempDir()
	dstDir := t.TempDir()

	const path = "prod/terraform.tfstate"
	srcContent := []byte(`{"version":4,"serial":9,"truth":"source"}`)

	srcBackend, err := storage.NewBackendFromRawConfig("local", localConfig(t, srcDir))
	require.NoError(t, err)
	dstBackend, err := storage.NewBackendFromRawConfig("local", localConfig(t, dstDir))
	require.NoError(t, err)
	require.NoError(t, srcBackend.Put(ctx, path, srcContent))
	// Target starts with stale, divergent bytes.
	require.NoError(t, dstBackend.Put(ctx, path, []byte(`{"version":4,"serial":1,"stale":true}`)))

	svc, mock := newExecService(t)

	job := &models.MigrationJob{
		ID:            "33333333-3333-3333-3333-333333333333",
		Name:          "divergent-target",
		SourceBackend: "local",
		SourceConfig:  localConfig(t, srcDir),
		TargetBackend: "local",
		TargetConfig:  localConfig(t, dstDir),
		Status:        models.MigrationStatusPending,
	}
	expectExecuteFlow(mock, job, 1)

	svc.executeMigration(ctx, job)

	assert.Equal(t, models.MigrationStatusCompleted, job.Status)
	assert.Equal(t, 1, job.MigratedFiles, "divergent target must be re-transferred")
	assert.Equal(t, 0, job.SkippedFiles)

	got, err := dstBackend.Get(ctx, path)
	require.NoError(t, err)
	assert.Equal(t, srcContent, got, "target must converge on source content")

	require.NoError(t, mock.ExpectationsWereMet())
}

// TestExecuteMigration_LocalToMinIO_EndToEnd migrates a state file from a local
// backend to an S3 (MinIO) backend, exercising a genuinely heterogeneous
// two-backend path. It is gated on TSM_TEST_MINIO_ENDPOINT and SKIPPED in CI
// (no MinIO service). To run locally:
//
//	docker run -p 9000:9000 -e MINIO_ROOT_USER=minioadmin \
//	  -e MINIO_ROOT_PASSWORD=minioadmin minio/minio server /data
//	# create the bucket once (e.g. via mc or the console), then:
//	TSM_TEST_MINIO_ENDPOINT=http://localhost:9000 \
//	TSM_TEST_MINIO_BUCKET=tsm-migrate-test \
//	TSM_TEST_MINIO_ACCESS_KEY=minioadmin \
//	TSM_TEST_MINIO_SECRET_KEY=minioadmin \
//	  go test ./internal/services/migration/ -run LocalToMinIO -v
//
// The S3 backend is created with force_path_style=true so MinIO's
// path-addressed bucket layout resolves correctly.
func TestExecuteMigration_LocalToMinIO_EndToEnd(t *testing.T) {
	endpoint := os.Getenv("TSM_TEST_MINIO_ENDPOINT")
	if endpoint == "" {
		t.Skip("TSM_TEST_MINIO_ENDPOINT not set; skipping local->S3 (MinIO) integration test")
	}

	bucket := envOrDefault("TSM_TEST_MINIO_BUCKET", "tsm-migrate-test")
	accessKey := envOrDefault("TSM_TEST_MINIO_ACCESS_KEY", "minioadmin")
	secretKey := envOrDefault("TSM_TEST_MINIO_SECRET_KEY", "minioadmin")
	region := envOrDefault("TSM_TEST_MINIO_REGION", "us-east-1")

	ctx := context.Background()
	srcDir := t.TempDir()

	// Use a unique key prefix per run so repeated local runs don't collide.
	prefix := fmt.Sprintf("migrate-test-%d", time.Now().UnixNano())

	const stateFile = "prod/terraform.tfstate"
	content := []byte(`{"version":4,"serial":11,"backend":"minio"}`)

	srcConfig := localConfig(t, srcDir)
	srcBackend, err := storage.NewBackendFromRawConfig("local", srcConfig)
	require.NoError(t, err)
	require.NoError(t, srcBackend.Put(ctx, stateFile, content))

	targetConfig, err := json.Marshal(map[string]interface{}{
		"bucket":            bucket,
		"region":            region,
		"endpoint":          endpoint,
		"access_key_id":     accessKey,
		"secret_access_key": secretKey,
		"prefix":            prefix,
		"force_path_style":  true,
	})
	require.NoError(t, err)

	// Sanity-check that the target backend is reachable before driving the job.
	targetBackend, err := storage.NewBackendFromRawConfig("s3", targetConfig)
	require.NoError(t, err)
	t.Cleanup(func() { _ = targetBackend.Delete(context.Background(), stateFile) })

	svc, mock := newExecService(t)

	job := &models.MigrationJob{
		ID:            "44444444-4444-4444-4444-444444444444",
		Name:          "local-to-minio",
		SourceBackend: "local",
		SourceConfig:  srcConfig,
		TargetBackend: "s3",
		TargetConfig:  targetConfig,
		Status:        models.MigrationStatusPending,
	}
	expectExecuteFlow(mock, job, 1)

	svc.executeMigration(ctx, job)

	require.Equal(t, models.MigrationStatusCompleted, job.Status,
		"local->MinIO migration must complete; error_log=%s", job.ErrorLog)
	assert.Equal(t, 1, job.MigratedFiles)
	assert.Equal(t, 0, job.FailedFiles)

	// The object must land in MinIO with byte-identical, checksum-matched content.
	got, err := targetBackend.Get(ctx, stateFile)
	require.NoError(t, err)
	assert.Equal(t, content, got)
	assert.Equal(t, sha256Hex(content), sha256Hex(got))

	require.NoError(t, mock.ExpectationsWereMet())
}

// envOrDefault returns the value of the named environment variable, or def when
// it is unset or empty.
func envOrDefault(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}
