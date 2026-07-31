package repositories

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Backup is a pre-edit copy of a state file. Data is omitted from JSON responses.
type Backup struct {
	ID        string `json:"id"`
	SourceID  string `json:"source_id"`
	StateKey  string `json:"state_key"`
	Data      []byte `json:"-"`
	Serial    *int64 `json:"serial"`
	CreatedBy string `json:"created_by"`
	CreatedAt string `json:"created_at"`
}

// Edit is an audit record of a state mutation.
type Edit struct {
	SourceID     string
	StateKey     string
	Operation    string
	Actor        string
	BackupID     *string
	BeforeSerial *int64
	AfterSerial  *int64
	Result       string
	Detail       string
}

// StateEditRepository persists backups and the edit audit trail.
type StateEditRepository struct {
	db *sql.DB
}

func NewStateEditRepository(db *sql.DB) *StateEditRepository {
	return &StateEditRepository{db: db}
}

// CreateBackup stores a copy of the current state and returns the backup id.
func (r *StateEditRepository) CreateBackup(ctx context.Context, sourceID, key string, data []byte, serial *int64, createdBy string) (string, error) {
	var id string
	err := r.db.QueryRowContext(ctx, `
		INSERT INTO state_backups (source_id, state_key, data, serial, created_by)
		VALUES ($1, $2, $3, $4, $5) RETURNING id`,
		sourceID, key, data, serial, nullStr(createdBy)).Scan(&id)
	return id, err
}

// ListBackups returns backup metadata (no data) for a source+key, newest first,
// bounded by limit/offset so a key with an unbounded backup history cannot be
// returned in full in a single response (#262).
func (r *StateEditRepository) ListBackups(ctx context.Context, sourceID, key string, limit, offset int) ([]Backup, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, source_id, state_key, serial, COALESCE(created_by, ''), created_at::text
		FROM state_backups WHERE source_id = $1 AND state_key = $2
		ORDER BY created_at DESC
		LIMIT $3 OFFSET $4`, sourceID, key, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	backups := []Backup{}
	for rows.Next() {
		var b Backup
		var serial sql.NullInt64
		if err := rows.Scan(&b.ID, &b.SourceID, &b.StateKey, &serial, &b.CreatedBy, &b.CreatedAt); err != nil {
			return nil, err
		}
		if serial.Valid {
			v := serial.Int64
			b.Serial = &v
		}
		backups = append(backups, b)
	}
	return backups, rows.Err()
}

// GetBackup returns a single backup including its data.
func (r *StateEditRepository) GetBackup(ctx context.Context, id string) (*Backup, error) {
	var b Backup
	var serial sql.NullInt64
	err := r.db.QueryRowContext(ctx, `
		SELECT id, source_id, state_key, data, serial, COALESCE(created_by, ''), created_at::text
		FROM state_backups WHERE id = $1`, id).
		Scan(&b.ID, &b.SourceID, &b.StateKey, &b.Data, &serial, &b.CreatedBy, &b.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if serial.Valid {
		v := serial.Int64
		b.Serial = &v
	}
	return &b, nil
}

// DeleteBackups removes every stored backup for a source+key — the purge path of
// an admin state delete. It returns the number of backups removed. The edit
// audit trail (state_edits) is intentionally left intact so the deletion stays
// accountable even after a purge.
func (r *StateEditRepository) DeleteBackups(ctx context.Context, sourceID, key string) (int64, error) {
	res, err := r.db.ExecContext(ctx,
		`DELETE FROM state_backups WHERE source_id = $1 AND state_key = $2`, sourceID, key)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// PruneBackups bounds the state_backups table (#257): it deletes backups older
// than maxAge EXCEPT the newest keep per (source_id, state_key). The keep floor
// is what makes an age cap safe here — a plain age DELETE would wipe every
// restore point for a state that simply has not been edited lately, so the
// floor guarantees a rarely-edited state keeps its most recent backups
// regardless of age. Returns the number of backups removed.
//
// keep must be >= 1 and maxAge > 0; the repository refuses anything else rather
// than trusting the caller, since a zero floor turns this into a full purge.
func (r *StateEditRepository) PruneBackups(ctx context.Context, keep int, maxAge time.Duration) (int64, error) {
	if keep < 1 {
		return 0, fmt.Errorf("backup retention keep must be >= 1, got %d", keep)
	}
	if maxAge <= 0 {
		return 0, fmt.Errorf("backup retention max age must be > 0, got %s", maxAge)
	}
	// The age is passed as seconds and turned into an interval by make_interval
	// server-side, so no interval literal is ever built by string concatenation.
	res, err := r.db.ExecContext(ctx, `
		DELETE FROM state_backups b
		USING (
			SELECT id, row_number() OVER (
				PARTITION BY source_id, state_key ORDER BY created_at DESC
			) AS rn
			FROM state_backups
		) r
		WHERE b.id = r.id
		  AND r.rn > $1
		  AND b.created_at < now() - make_interval(secs => $2)`,
		keep, maxAge.Seconds())
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// RecordEdit appends to the edit audit trail.
func (r *StateEditRepository) RecordEdit(ctx context.Context, e *Edit) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO state_edits
			(source_id, state_key, operation, actor, backup_id, before_serial, after_serial, result, detail)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		e.SourceID, e.StateKey, e.Operation, nullStr(e.Actor), e.BackupID, e.BeforeSerial, e.AfterSerial, e.Result, nullStr(e.Detail))
	return err
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}
