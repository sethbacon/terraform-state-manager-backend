package repositories

import (
	"context"
	"database/sql"
	"time"

	"github.com/lib/pq"
)

// NotificationChannel is a destination for alerts. The target URL is held
// encrypted (EncryptedTarget) and never serialized; HasTarget reports whether one
// is set so the UI can show "configured" without exposing the secret.
type NotificationChannel struct {
	ID              string   `json:"id"`
	Name            string   `json:"name"`
	Type            string   `json:"type"` // webhook | slack
	EncryptedTarget []byte   `json:"-"`
	HasTarget       bool     `json:"has_target"`
	Events          []string `json:"events"` // empty = all events
	Enabled         bool     `json:"enabled"`
	LastStatus      *string  `json:"last_status"`
	LastError       *string  `json:"last_error"`
	LastSentAt      *string  `json:"last_sent_at"`
	CreatedAt       string   `json:"created_at"`
	UpdatedAt       string   `json:"updated_at"`
}

// NotificationChannelRepository is the DAO for notification_channels.
type NotificationChannelRepository struct {
	db *sql.DB
}

func NewNotificationChannelRepository(db *sql.DB) *NotificationChannelRepository {
	return &NotificationChannelRepository{db: db}
}

const channelColumns = `id, name, type, encrypted_target, events, enabled,
	last_status, last_error, last_sent_at::text, created_at::text, updated_at::text`

func scanChannel(scanner interface{ Scan(dest ...any) error }) (*NotificationChannel, error) {
	var ch NotificationChannel
	var target []byte
	var events pq.StringArray
	var lastStatus, lastError, lastSentAt sql.NullString
	if err := scanner.Scan(&ch.ID, &ch.Name, &ch.Type, &target, &events, &ch.Enabled,
		&lastStatus, &lastError, &lastSentAt, &ch.CreatedAt, &ch.UpdatedAt); err != nil {
		return nil, err
	}
	ch.EncryptedTarget = target
	ch.HasTarget = len(target) > 0
	ch.Events = []string(events)
	if ch.Events == nil {
		ch.Events = []string{}
	}
	if lastStatus.Valid {
		ch.LastStatus = &lastStatus.String
	}
	if lastError.Valid {
		ch.LastError = &lastError.String
	}
	if lastSentAt.Valid {
		ch.LastSentAt = &lastSentAt.String
	}
	return &ch, nil
}

func (r *NotificationChannelRepository) Create(ctx context.Context, ch *NotificationChannel) (*NotificationChannel, error) {
	row := r.db.QueryRowContext(ctx, `
		INSERT INTO notification_channels (name, type, encrypted_target, events, enabled)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING `+channelColumns,
		ch.Name, ch.Type, ch.EncryptedTarget, pq.Array(ch.Events), ch.Enabled)
	saved, err := scanChannel(row)
	if err != nil {
		return nil, err
	}
	saved.EncryptedTarget = nil // never expose the secret to callers
	return saved, nil
}

// List returns all channels without the encrypted target (for the admin UI).
func (r *NotificationChannelRepository) List(ctx context.Context) ([]NotificationChannel, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT `+channelColumns+` FROM notification_channels ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []NotificationChannel{}
	for rows.Next() {
		ch, err := scanChannel(rows)
		if err != nil {
			return nil, err
		}
		ch.EncryptedTarget = nil
		out = append(out, *ch)
	}
	return out, rows.Err()
}

// GetByID returns a channel including its encrypted target (for decryption by the
// notifier / test endpoint).
func (r *NotificationChannelRepository) GetByID(ctx context.Context, id string) (*NotificationChannel, error) {
	row := r.db.QueryRowContext(ctx, `SELECT `+channelColumns+` FROM notification_channels WHERE id = $1`, id)
	ch, err := scanChannel(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return ch, nil
}

// Update replaces the mutable fields. When encryptedTarget is nil the existing
// target is kept (so editing a channel without re-entering the URL is allowed).
func (r *NotificationChannelRepository) Update(ctx context.Context, id, name, typ string, events []string, enabled bool, encryptedTarget []byte) (*NotificationChannel, error) {
	var targetArg any
	if len(encryptedTarget) > 0 {
		targetArg = encryptedTarget
	}
	row := r.db.QueryRowContext(ctx, `
		UPDATE notification_channels
		SET name=$2, type=$3, events=$4, enabled=$5,
		    encrypted_target=COALESCE($6, encrypted_target), updated_at=now()
		WHERE id=$1
		RETURNING `+channelColumns,
		id, name, typ, pq.Array(events), enabled, targetArg)
	ch, err := scanChannel(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	ch.EncryptedTarget = nil
	return ch, nil
}

func (r *NotificationChannelRepository) Delete(ctx context.Context, id string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM notification_channels WHERE id = $1`, id)
	return err
}

// ListEnabledForEvent returns enabled channels subscribed to eventType (a channel
// with no events subscribes to all). Includes the encrypted target for sending.
func (r *NotificationChannelRepository) ListEnabledForEvent(ctx context.Context, eventType string) ([]NotificationChannel, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT `+channelColumns+`
		FROM notification_channels
		WHERE enabled AND (cardinality(events) = 0 OR $1 = ANY(events))`, eventType)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []NotificationChannel{}
	for rows.Next() {
		ch, err := scanChannel(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *ch)
	}
	return out, rows.Err()
}

// RecordDelivery stamps the outcome of the most recent send attempt.
func (r *NotificationChannelRepository) RecordDelivery(ctx context.Context, id, status, errMsg string, sentAt time.Time) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE notification_channels SET last_status=$2, last_error=NULLIF($3,''), last_sent_at=$4, updated_at=now() WHERE id=$1`,
		id, status, errMsg, sentAt)
	return err
}
