package repositories

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/terraform-state-manager/terraform-state-manager/internal/db/models"
)

// uiThemeSettingKey is the system_settings key under which the white-label theme
// configuration JSON blob is stored.
const uiThemeSettingKey = "ui_theme"

// UIThemeRepository persists the singleton white-label theme configuration in the
// system_settings key-value table (key "ui_theme", JSON-encoded value). Reusing
// system_settings avoids a dedicated table and migration for a single-row config.
type UIThemeRepository struct {
	db *sqlx.DB
}

// NewUIThemeRepository constructs a UIThemeRepository over the given connection.
func NewUIThemeRepository(db *sqlx.DB) *UIThemeRepository {
	return &UIThemeRepository{db: db}
}

// Get returns the stored theme configuration, or nil if none has been written
// yet so the caller can fall back to a built-in default.
func (r *UIThemeRepository) Get(ctx context.Context) (*models.UIThemeConfig, error) {
	var value string
	err := r.db.GetContext(ctx, &value,
		"SELECT value FROM system_settings WHERE key = $1", uiThemeSettingKey)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to load ui theme: %w", err)
	}

	var cfg models.UIThemeConfig
	if err := json.Unmarshal([]byte(value), &cfg); err != nil {
		return nil, fmt.Errorf("failed to decode ui theme: %w", err)
	}
	return &cfg, nil
}

// Upsert writes (or replaces) the theme configuration and returns the saved row
// with its UpdatedAt timestamp set.
func (r *UIThemeRepository) Upsert(ctx context.Context, in *models.UIThemeConfig) (*models.UIThemeConfig, error) {
	saved := *in
	saved.UpdatedAt = time.Now().UTC()

	payload, err := json.Marshal(&saved)
	if err != nil {
		return nil, fmt.Errorf("failed to encode ui theme: %w", err)
	}

	_, err = r.db.ExecContext(ctx,
		`INSERT INTO system_settings (key, value, updated_at) VALUES ($1, $2, NOW())
		 ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = NOW()`,
		uiThemeSettingKey, string(payload),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to save ui theme: %w", err)
	}
	return &saved, nil
}
