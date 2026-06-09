package repositories

import (
	"context"
	"database/sql"
	"errors"

	"github.com/lib/pq"
)

// ErrLocked is returned by StateLockRepository.Acquire when the (source, key) is
// already locked by another editor.
var ErrLocked = errors.New("state is locked by another operation")

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
// ErrLocked. This is atomic at the database — no read-then-write race.
func (r *StateLockRepository) Acquire(ctx context.Context, sourceID, key, actor string) (string, error) {
	var id string
	err := r.db.QueryRowContext(ctx,
		`INSERT INTO state_locks (source_id, state_key, actor) VALUES ($1, $2, $3) RETURNING id`,
		sourceID, key, actor).Scan(&id)
	if err != nil {
		var pqErr *pq.Error
		if errors.As(err, &pqErr) && pqErr.Code == "23505" { // unique_violation
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
