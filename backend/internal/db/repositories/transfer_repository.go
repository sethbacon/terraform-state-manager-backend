package repositories

import (
	"context"
	"database/sql"
	"errors"
)

// Transfer records a cross-source backup (copy) or migrate (move + verify).
type Transfer struct {
	ID             string `json:"id"`
	Mode           string `json:"mode"`
	SourceID       string `json:"source_id"`
	SourceKey      string `json:"source_key"`
	TargetSourceID string `json:"target_source_id"`
	TargetKey      string `json:"target_key"`
	Status         string `json:"status"`
	Verified       *bool  `json:"verified"`
	Decommissioned bool   `json:"decommissioned"`
	Detail         string `json:"detail"`
	Actor          string `json:"actor"`
	CreatedAt      string `json:"created_at"`
}

// TransferRepository persists transfer records.
type TransferRepository struct {
	db *sql.DB
}

func NewTransferRepository(db *sql.DB) *TransferRepository {
	return &TransferRepository{db: db}
}

const transferColumns = `id, mode, source_id, source_key, target_source_id, target_key, status, verified, decommissioned, COALESCE(detail, ''), COALESCE(actor, ''), created_at::text`

func scanTransfer(scanner interface{ Scan(dest ...any) error }) (*Transfer, error) {
	var t Transfer
	var verified sql.NullBool
	if err := scanner.Scan(&t.ID, &t.Mode, &t.SourceID, &t.SourceKey, &t.TargetSourceID, &t.TargetKey,
		&t.Status, &verified, &t.Decommissioned, &t.Detail, &t.Actor, &t.CreatedAt); err != nil {
		return nil, err
	}
	if verified.Valid {
		v := verified.Bool
		t.Verified = &v
	}
	return &t, nil
}

// Create inserts a transfer record and returns it.
func (r *TransferRepository) Create(ctx context.Context, t *Transfer) (*Transfer, error) {
	row := r.db.QueryRowContext(ctx, `
		INSERT INTO state_transfers
			(mode, source_id, source_key, target_source_id, target_key, status, verified, decommissioned, detail, actor)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		RETURNING `+transferColumns,
		t.Mode, t.SourceID, t.SourceKey, t.TargetSourceID, t.TargetKey, t.Status, t.Verified, t.Decommissioned,
		nullStr(t.Detail), nullStr(t.Actor))
	return scanTransfer(row)
}

// GetByID returns a transfer or (nil, nil) when not found.
func (r *TransferRepository) GetByID(ctx context.Context, id string) (*Transfer, error) {
	row := r.db.QueryRowContext(ctx, `SELECT `+transferColumns+` FROM state_transfers WHERE id = $1`, id)
	t, err := scanTransfer(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return t, nil
}
