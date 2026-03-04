package repositories

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/terraform-state-manager/terraform-state-manager/internal/db/models"
)

// NotificationChannelRepository handles database operations for notification channels.
type NotificationChannelRepository struct {
	db *sql.DB
}

// NewNotificationChannelRepository creates a new NotificationChannelRepository.
func NewNotificationChannelRepository(db *sql.DB) *NotificationChannelRepository {
	return &NotificationChannelRepository{db: db}
}

// Create inserts a new notification channel into the database.
func (r *NotificationChannelRepository) Create(ctx context.Context, channel *models.NotificationChannel) error {
	err := r.db.QueryRowContext(ctx,
		`INSERT INTO notification_channels (organization_id, name, channel_type, config, is_active)
		 VALUES ($1, $2, $3, $4, $5)
		 RETURNING id, created_at, updated_at`,
		channel.OrganizationID,
		channel.Name,
		channel.ChannelType,
		channel.Config,
		channel.IsActive,
	).Scan(&channel.ID, &channel.CreatedAt, &channel.UpdatedAt)
	if err != nil {
		return fmt.Errorf("failed to create notification channel: %w", err)
	}
	return nil
}

// GetByID retrieves a notification channel by its ID.
func (r *NotificationChannelRepository) GetByID(ctx context.Context, id string) (*models.NotificationChannel, error) {
	var ch models.NotificationChannel
	err := r.db.QueryRowContext(ctx,
		`SELECT id, organization_id, name, channel_type, config, is_active, created_at, updated_at
		 FROM notification_channels
		 WHERE id = $1`,
		id,
	).Scan(
		&ch.ID,
		&ch.OrganizationID,
		&ch.Name,
		&ch.ChannelType,
		&ch.Config,
		&ch.IsActive,
		&ch.CreatedAt,
		&ch.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get notification channel by ID: %w", err)
	}
	return &ch, nil
}

// ListByOrganization returns paginated notification channels for a given organization, along with the total count.
func (r *NotificationChannelRepository) ListByOrganization(ctx context.Context, orgID string, limit, offset int) ([]models.NotificationChannel, int, error) {
	var total int
	err := r.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM notification_channels WHERE organization_id = $1",
		orgID,
	).Scan(&total)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to count notification channels: %w", err)
	}

	rows, err := r.db.QueryContext(ctx,
		`SELECT id, organization_id, name, channel_type, config, is_active, created_at, updated_at
		 FROM notification_channels
		 WHERE organization_id = $1
		 ORDER BY name
		 LIMIT $2 OFFSET $3`,
		orgID, limit, offset,
	)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to list notification channels: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var channels []models.NotificationChannel
	for rows.Next() {
		var ch models.NotificationChannel
		if err := rows.Scan(
			&ch.ID,
			&ch.OrganizationID,
			&ch.Name,
			&ch.ChannelType,
			&ch.Config,
			&ch.IsActive,
			&ch.CreatedAt,
			&ch.UpdatedAt,
		); err != nil {
			return nil, 0, fmt.Errorf("failed to scan notification channel: %w", err)
		}
		channels = append(channels, ch)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("failed to iterate notification channels: %w", err)
	}
	return channels, total, nil
}

// Update modifies an existing notification channel.
func (r *NotificationChannelRepository) Update(ctx context.Context, channel *models.NotificationChannel) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE notification_channels
		 SET name = $1, channel_type = $2, config = $3, is_active = $4, updated_at = $5
		 WHERE id = $6`,
		channel.Name,
		channel.ChannelType,
		channel.Config,
		channel.IsActive,
		time.Now(),
		channel.ID,
	)
	if err != nil {
		return fmt.Errorf("failed to update notification channel: %w", err)
	}
	return nil
}

// Delete removes a notification channel by its ID.
func (r *NotificationChannelRepository) Delete(ctx context.Context, id string) error {
	_, err := r.db.ExecContext(ctx, "DELETE FROM notification_channels WHERE id = $1", id)
	if err != nil {
		return fmt.Errorf("failed to delete notification channel: %w", err)
	}
	return nil
}
