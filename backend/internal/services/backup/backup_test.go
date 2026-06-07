package backup

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
	"github.com/terraform-state-manager/terraform-state-manager/internal/storage/local"
)

// backupGetByIDColumns mirrors the column order scanned by
// BackupRepository.GetByID. Keep in sync with backup_repository.go.
var backupGetByIDColumns = []string{
	"id", "organization_id", "source_id", "workspace_name", "workspace_id",
	"storage_backend", "storage_path", "file_size_bytes", "terraform_version",
	"state_serial", "checksum_sha256", "retention_policy_id", "expires_at", "created_at",
}

// newServiceWithMock builds a backup Service whose repository is backed by a
// sqlmock database (no real Postgres) and whose storage is a local backend
// rooted at a fresh temp dir. It returns the service, the mock, and the backend.
func newServiceWithMock(t *testing.T) (*Service, sqlmock.Sqlmock, *local.Backend) {
	t.Helper()

	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	store, err := local.NewBackend(t.TempDir())
	require.NoError(t, err)

	backupRepo := repositories.NewBackupRepository(db)
	svc := NewService(backupRepo, nil, nil, store)
	return svc, mock, store
}

// expectGetByID queues a sqlmock expectation returning a single backup row with
// the given storage path and checksum. A nil checksum yields a NULL column.
func expectGetByID(mock sqlmock.Sqlmock, id, storagePath string, checksum *string) {
	row := sqlmock.NewRows(backupGetByIDColumns).AddRow(
		id,          // id
		"org-1",     // organization_id
		nil,         // source_id
		"prod",      // workspace_name
		nil,         // workspace_id
		"default",   // storage_backend
		storagePath, // storage_path
		int64(0),    // file_size_bytes
		nil,         // terraform_version
		nil,         // state_serial
		checksum,    // checksum_sha256
		nil,         // retention_policy_id
		nil,         // expires_at
		time.Now(),  // created_at
	)
	mock.ExpectQuery("SELECT .* FROM state_backups").WithArgs(id).WillReturnRows(row)
}

func TestVerifyRestoreRoundTrip_Valid(t *testing.T) {
	ctx := context.Background()
	svc, mock, store := newServiceWithMock(t)

	const id = "11111111-1111-1111-1111-111111111111"
	const path = "backups/org-1/prod/state.tfstate"
	content := []byte(`{"version":4,"serial":42}`)
	require.NoError(t, store.Put(ctx, path, content))

	checksum := sha256Hex(content)
	expectGetByID(mock, id, path, &checksum)

	valid, data, backup, err := svc.VerifyRestoreRoundTrip(ctx, id)
	require.NoError(t, err)
	assert.True(t, valid, "intact backup must verify as valid")
	assert.Equal(t, content, data, "original bytes must be returned")
	require.NotNil(t, backup)
	assert.Equal(t, id, backup.ID)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestVerifyRestoreRoundTrip_Mutated(t *testing.T) {
	ctx := context.Background()
	svc, mock, store := newServiceWithMock(t)

	const id = "22222222-2222-2222-2222-222222222222"
	const path = "backups/org-1/prod/state.tfstate"
	original := []byte(`{"version":4,"serial":42}`)

	// Record the checksum of the original, but store mutated bytes on disk.
	checksum := sha256Hex(original)
	require.NoError(t, store.Put(ctx, path, []byte(`{"version":4,"serial":99}`)))
	expectGetByID(mock, id, path, &checksum)

	valid, data, _, err := svc.VerifyRestoreRoundTrip(ctx, id)
	require.NoError(t, err)
	assert.False(t, valid, "mutated stored object must verify as invalid")
	assert.NotEmpty(t, data, "data is still returned for inspection")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestVerifyRestoreRoundTrip_NoChecksum(t *testing.T) {
	ctx := context.Background()
	svc, mock, store := newServiceWithMock(t)

	const id = "33333333-3333-3333-3333-333333333333"
	const path = "backups/org-1/prod/state.tfstate"
	require.NoError(t, store.Put(ctx, path, []byte("anything")))

	// Backup record has a NULL checksum_sha256.
	expectGetByID(mock, id, path, nil)

	valid, _, _, err := svc.VerifyRestoreRoundTrip(ctx, id)
	require.Error(t, err, "missing stored checksum must be an error")
	assert.False(t, valid)
	assert.Contains(t, err.Error(), "no stored checksum")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestVerifyRestoreRoundTrip_NotFound(t *testing.T) {
	ctx := context.Background()
	svc, mock, _ := newServiceWithMock(t)

	const id = "44444444-4444-4444-4444-444444444444"
	mock.ExpectQuery("SELECT .* FROM state_backups").
		WithArgs(id).
		WillReturnError(sqlmock.ErrCancelled)

	valid, _, _, err := svc.VerifyRestoreRoundTrip(ctx, id)
	require.Error(t, err)
	assert.False(t, valid)
}
