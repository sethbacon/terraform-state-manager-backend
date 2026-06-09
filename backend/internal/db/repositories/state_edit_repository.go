package repositories

import (
	"context"
	"database/sql"
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

// ListBackups returns backup metadata (no data) for a source+key, newest first.
func (r *StateEditRepository) ListBackups(ctx context.Context, sourceID, key string) ([]Backup, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, source_id, state_key, serial, COALESCE(created_by, ''), created_at::text
		FROM state_backups WHERE source_id = $1 AND state_key = $2
		ORDER BY created_at DESC`, sourceID, key)
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
	if err == sql.ErrNoRows {
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
