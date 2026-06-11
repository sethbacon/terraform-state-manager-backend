package repositories

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/lib/pq"
)

// ErrLocked is returned by StateLockRepository.Acquire when the (source, key) is
// already locked by another editor.
var ErrLocked = errors.New("state is locked by another operation")

// staleLockTTL bounds how long an orphaned lock can wedge a key. Locks are
// request-scoped (acquired and released within one HTTP request), so any lock
// older than this belongs to a crashed process and is reaped on the next
// Acquire. Postgres interval syntax.
const staleLockTTL = "15 minutes"

// StateLockRepository implements app-level advisory locks over state_locks. It
// guarantees mutual exclusion for any source, including connectors that have no
// native backend lock (S3/GCS/Azure/HCP/git).
type StateLockRepository struct {
	db *sql.DB
}

func NewStateLockRepository(db *sql.DB) *StateLockRepository {
	return &StateLockRepository{db: db}
}

// Acquire inserts a lock row, returning its id. If the (source_id, state_key) is
// already held, the UNIQUE constraint rejects the insert and Acquire returns
// ErrLocked (annotated with the holder when known). This is atomic at the
// database — no read-then-write race. Locks past staleLockTTL are reaped first
// so a crash between Acquire and Release cannot wedge the key forever.
func (r *StateLockRepository) Acquire(ctx context.Context, sourceID, key, actor string) (string, error) {
	if _, err := r.db.ExecContext(ctx,
		`DELETE FROM state_locks WHERE source_id = $1 AND state_key = $2 AND acquired_at < now() - $3::interval`,
		sourceID, key, staleLockTTL); err != nil {
		return "", err
	}
	var id string
	err := r.db.QueryRowContext(ctx,
		`INSERT INTO state_locks (source_id, state_key, actor) VALUES ($1, $2, $3) RETURNING id`,
		sourceID, key, actor).Scan(&id)
	if err != nil {
		var pqErr *pq.Error
		if errors.As(err, &pqErr) && pqErr.Code == "23505" { // unique_violation
			// Name the holder so operators can decide whether to force-unlock.
			var holder, since string
			if qErr := r.db.QueryRowContext(ctx,
				`SELECT COALESCE(actor, ''), acquired_at::text FROM state_locks WHERE source_id = $1 AND state_key = $2`,
				sourceID, key).Scan(&holder, &since); qErr == nil {
				return "", fmt.Errorf("%w (held by %q since %s)", ErrLocked, holder, since)
			}
			return "", ErrLocked
		}
		return "", err
	}
	return id, nil
}

// Release removes the lock row identified by lockID (scoped to source/key).
func (r *StateLockRepository) Release(ctx context.Context, sourceID, key, lockID string) error {
	_, err := r.db.ExecContext(ctx,
		`DELETE FROM state_locks WHERE id = $1 AND source_id = $2 AND state_key = $3`,
		lockID, sourceID, key)
	return err
}

// ForceRelease removes any lock on (source, key) regardless of holder — the
// admin escape hatch for locks orphaned by a crash that have not yet aged past
// staleLockTTL. Returns whether a lock existed.
func (r *StateLockRepository) ForceRelease(ctx context.Context, sourceID, key string) (bool, error) {
	res, err := r.db.ExecContext(ctx,
		`DELETE FROM state_locks WHERE source_id = $1 AND state_key = $2`,
		sourceID, key)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}
