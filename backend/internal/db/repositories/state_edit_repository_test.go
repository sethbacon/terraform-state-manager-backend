package repositories

import (
	"errors"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
)

// ---------------------------------------------------------------------------
// StateEditRepository — backup retention (#257)
// ---------------------------------------------------------------------------

func TestPruneBackups(t *testing.T) {
	db, mock := newMock(t)
	r := NewStateEditRepository(db)

	// keep floor is passed as-is; max age is passed in seconds so the interval
	// is built server-side by make_interval rather than string-concatenated.
	mock.ExpectExec("DELETE FROM state_backups").
		WithArgs(20, 90*24*time.Hour.Seconds()).
		WillReturnResult(sqlmock.NewResult(0, 7))

	n, err := r.PruneBackups(ctx, 20, 90*24*time.Hour)
	if err != nil {
		t.Fatalf("PruneBackups: %v", err)
	}
	if n != 7 {
		t.Errorf("deleted = %d, want 7", n)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// A keep floor below 1 would let the age cap delete a state's last restore
// point. The repository refuses rather than trusting its caller, so a future
// caller that skips config validation still cannot purge every backup.
func TestPruneBackupsRejectsUnsafeKeep(t *testing.T) {
	db, mock := newMock(t)
	r := NewStateEditRepository(db)

	for _, keep := range []int{0, -1} {
		if _, err := r.PruneBackups(ctx, keep, time.Hour); err == nil {
			t.Errorf("keep=%d: expected an error, got nil", keep)
		}
	}
	// No statement may reach the database on the refused path.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestPruneBackupsRejectsNonPositiveMaxAge(t *testing.T) {
	db, _ := newMock(t)
	r := NewStateEditRepository(db)

	if _, err := r.PruneBackups(ctx, 20, 0); err == nil {
		t.Error("max age 0 must be refused (it would delete every backup past the floor)")
	}
}

func TestPruneBackupsPropagatesError(t *testing.T) {
	db, mock := newMock(t)
	r := NewStateEditRepository(db)

	mock.ExpectExec("DELETE FROM state_backups").
		WillReturnError(errors.New("db down"))

	if _, err := r.PruneBackups(ctx, 20, time.Hour); err == nil {
		t.Error("expected the database error to propagate")
	}
}
