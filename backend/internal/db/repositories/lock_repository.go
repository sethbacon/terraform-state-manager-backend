package repositories

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/lib/pq"
)

// ErrLocked is returned by StateLockRepository.Acquire when the (source, key) is
// already locked by another editor.
var ErrLocked = errors.New("state is locked by another operation")

// staleLockTTL bounds how long an orphaned lock can wedge a key. Reaping keys
// on the renewed_at heartbeat, not acquisition age: a holder renews while it
// works (KeepAlive), so only a lock whose holder stopped renewing — i.e. a
// crashed process — ages past this and is reaped on the next Acquire. Postgres
// interval syntax.
const staleLockTTL = "15 minutes"

// LockRenewInterval is how often a live holder renews its heartbeat — well
// under staleLockTTL so a healthy long-running operation can never age out
// (it would take three consecutive missed renewals).
const LockRenewInterval = 5 * time.Minute

// StateLockRepository implements app-level advisory locks over state_locks. It
// guarantees mutual exclusion for any source, including connectors that have no
// native backend lock (S3/GCS/Azure/HCP/git).
type StateLockRepository struct {
	db *sql.DB
}

func NewStateLockRepository(db *sql.DB) *StateLockRepository {
	return &StateLockRepository{db: db}
}

// StateLock is a currently-held advisory lock row. Age judgement (vs
// staleLockTTL) is left to the caller — the row carries the raw acquisition
// time, not a computed status.
type StateLock struct {
	ID         string `json:"id"`
	SourceID   string `json:"source_id"`
	StateKey   string `json:"state_key"`
	Actor      string `json:"actor"`
	AcquiredAt string `json:"acquired_at"`
}

// List returns the advisory locks currently held for a source, newest first.
func (r *StateLockRepository) List(ctx context.Context, sourceID string) ([]StateLock, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, source_id, state_key, COALESCE(actor, ''), acquired_at::text
		 FROM state_locks WHERE source_id = $1 ORDER BY acquired_at DESC`,
		sourceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	locks := []StateLock{}
	for rows.Next() {
		var l StateLock
		if err := rows.Scan(&l.ID, &l.SourceID, &l.StateKey, &l.Actor, &l.AcquiredAt); err != nil {
			return nil, err
		}
		locks = append(locks, l)
	}
	return locks, rows.Err()
}

// Acquire inserts a lock row, returning its id. If the (source_id, state_key) is
// already held, the UNIQUE constraint rejects the insert and Acquire returns
// ErrLocked (annotated with the holder when known). This is atomic at the
// database — no read-then-write race. Locks whose HEARTBEAT (renewed_at) aged
// past staleLockTTL are reaped first, so a crash between Acquire and Release
// cannot wedge the key forever — while a live long-running holder, which keeps
// renewing, is never reaped out from under its operation.
func (r *StateLockRepository) Acquire(ctx context.Context, sourceID, key, actor string) (string, error) {
	if _, err := r.db.ExecContext(ctx,
		`DELETE FROM state_locks WHERE source_id = $1 AND state_key = $2 AND renewed_at < now() - $3::interval`,
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

// Renew refreshes the lock's heartbeat. False (no error) means the row is gone
// — force-released or reaped — so the caller's operation no longer holds the
// lock and renewing must stop.
func (r *StateLockRepository) Renew(ctx context.Context, sourceID, key, lockID string) (bool, error) {
	res, err := r.db.ExecContext(ctx,
		`UPDATE state_locks SET renewed_at = now() WHERE id = $1 AND source_id = $2 AND state_key = $3`,
		lockID, sourceID, key)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// KeepAlive renews the lock's heartbeat every interval until stop is closed or
// the lock row disappears. Run it in a goroutine alongside a long operation;
// close stop before Release. Renewal errors are tolerated (transient DB blips
// must not kill the heartbeat — the next tick retries within the TTL budget).
func (r *StateLockRepository) KeepAlive(sourceID, key, lockID string, interval time.Duration, stop <-chan struct{}) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			alive, err := r.Renew(ctx, sourceID, key, lockID)
			cancel()
			if err == nil && !alive {
				return
			}
		case <-stop:
			return
		}
	}
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
