// system_settings_repository.go reads/writes the single-row system_settings
// table (id = 1) that holds first-run setup-wizard state. It lives in TSM's
// app/domain DB so a standalone TSM owns its setup state without ever writing
// the shared identity schema.
package repositories

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// SetupStatus is the first-run setup state surfaced to the setup wizard.
type SetupStatus struct {
	SetupCompleted    bool
	AdminConfigured   bool
	OIDCConfigured    bool
	LDAPConfigured    bool
	SourcesConfigured bool
	AuthMethod        string
}

// SystemSettingsRepository is the DAO for the system_settings singleton.
type SystemSettingsRepository struct {
	db *sql.DB
}

// NewSystemSettingsRepository constructs the repository over the app/domain DB.
func NewSystemSettingsRepository(db *sql.DB) *SystemSettingsRepository {
	return &SystemSettingsRepository{db: db}
}

// IsSetupCompleted reports whether first-run setup has finished.
func (r *SystemSettingsRepository) IsSetupCompleted(ctx context.Context) (bool, error) {
	var done bool
	err := r.db.QueryRowContext(ctx, `SELECT setup_completed FROM system_settings WHERE id = 1`).Scan(&done)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return done, err
}

// HasPendingFeatureSetup reports whether a feature added in a later release still
// needs configuring after initial setup completed. v1 has none; kept so the gate
// and status payload are forward-compatible without a schema change.
func (r *SystemSettingsRepository) HasPendingFeatureSetup(_ context.Context) (bool, error) {
	return false, nil
}

// GetSetupTokenHash returns the bcrypt hash of the one-time setup token, or "".
func (r *SystemSettingsRepository) GetSetupTokenHash(ctx context.Context) (string, error) {
	var hash sql.NullString
	err := r.db.QueryRowContext(ctx, `SELECT setup_token_hash FROM system_settings WHERE id = 1`).Scan(&hash)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if !hash.Valid {
		return "", nil
	}
	return hash.String, nil
}

// SetSetupTokenHash stores the bcrypt hash of the setup token.
func (r *SystemSettingsRepository) SetSetupTokenHash(ctx context.Context, hash string) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE system_settings SET setup_token_hash = $1, updated_at = $2 WHERE id = 1`, hash, time.Now())
	return err
}

// SetAdminConfigured records that the first owner has been created.
func (r *SystemSettingsRepository) SetAdminConfigured(ctx context.Context) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE system_settings SET admin_configured = true, updated_at = $1 WHERE id = 1`, time.Now())
	return err
}

// SetSourcesConfigured records that at least one state source has been added.
func (r *SystemSettingsRepository) SetSourcesConfigured(ctx context.Context) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE system_settings SET sources_configured = true, updated_at = $1 WHERE id = 1`, time.Now())
	return err
}

// SetOIDCConfigured records that OIDC is configured and sets the auth method.
func (r *SystemSettingsRepository) SetOIDCConfigured(ctx context.Context) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE system_settings SET oidc_configured = true, auth_method = 'oidc', updated_at = $1 WHERE id = 1`,
		time.Now())
	return err
}

// SetSetupCompleted marks setup complete and clears the token — the permanent,
// GitOps-safe self-disable (the wizard middleware then 403s on every endpoint).
func (r *SystemSettingsRepository) SetSetupCompleted(ctx context.Context) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE system_settings SET setup_completed = true, setup_token_hash = NULL, updated_at = $1 WHERE id = 1`,
		time.Now())
	return err
}

// GetStatus returns the current setup flags for the public status endpoint.
func (r *SystemSettingsRepository) GetStatus(ctx context.Context) (SetupStatus, error) {
	var s SetupStatus
	var auth sql.NullString
	err := r.db.QueryRowContext(ctx,
		`SELECT setup_completed, admin_configured, oidc_configured, ldap_configured, sources_configured, auth_method
		   FROM system_settings WHERE id = 1`).
		Scan(&s.SetupCompleted, &s.AdminConfigured, &s.OIDCConfigured, &s.LDAPConfigured, &s.SourcesConfigured, &auth)
	if errors.Is(err, sql.ErrNoRows) {
		return SetupStatus{}, nil
	}
	if err != nil {
		return SetupStatus{}, err
	}
	s.AuthMethod = auth.String
	return s, nil
}

// GetNotificationsConfig retrieves the persisted notifications/SMTP
// configuration JSON (may be nil if never saved). Mirrors terraform-registry's
// OIDCConfigRepository.GetNotificationsConfig for parity.
func (r *SystemSettingsRepository) GetNotificationsConfig(ctx context.Context) ([]byte, error) {
	var configJSON []byte
	err := r.db.QueryRowContext(ctx, `SELECT notifications_config FROM system_settings WHERE id = 1`).Scan(&configJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return configJSON, nil
}

// SetNotificationsConfig stores the notifications configuration JSON (the SMTP
// password MUST be encrypted by the caller via internal/crypto before it
// reaches here) and marks notifications as configured.
func (r *SystemSettingsRepository) SetNotificationsConfig(ctx context.Context, configJSON []byte) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE system_settings SET notifications_configured = true, notifications_config = $1, updated_at = $2 WHERE id = 1`,
		configJSON, time.Now())
	return err
}

// GetUIThemeConfig retrieves the persisted whitelabel branding JSON (nil when
// never saved) — the payload GET /api/v1/ui/theme serves.
func (r *SystemSettingsRepository) GetUIThemeConfig(ctx context.Context) ([]byte, error) {
	var configJSON []byte
	err := r.db.QueryRowContext(ctx, `SELECT ui_theme_config FROM system_settings WHERE id = 1`).Scan(&configJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return configJSON, nil
}

// SetUIThemeConfig stores the whitelabel branding JSON (validated by the API
// layer; contains no secrets).
func (r *SystemSettingsRepository) SetUIThemeConfig(ctx context.Context, configJSON []byte) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE system_settings SET ui_theme_config = $1, updated_at = $2 WHERE id = 1`,
		configJSON, time.Now())
	return err
}

// DefaultOrganizationID returns the organization migration 000033 records as this
// deployment's default, or "" when it has not been set.
//
// READ FROM THE APP CONNECTION, not by looking up an organization named
// "default" in identity, and the difference is not stylistic:
//
//   - It survives a RENAME. identity's OrganizationRepository has a Rename, and
//     after one a lookup by the name "default" returns not-found while this
//     column still holds the correct uuid.
//   - It keeps a cross-database read off a write path. The carrier lives on the
//     same connection the INSERT uses; the identity database may be a different
//     database entirely.
//   - It is guaranteed populated before any request is served. bootstrap.Run
//     writes it, main fails fatally if that errors, and the listener starts
//     afterwards.
//
// The one caller is the setup wizard, which creates a source with no principal
// at all and therefore has no acting organization to read (#436).
func (r *SystemSettingsRepository) DefaultOrganizationID(ctx context.Context) (string, error) {
	var orgID sql.NullString
	err := r.db.QueryRowContext(ctx,
		`SELECT default_organization_id FROM system_settings WHERE id = 1`).Scan(&orgID)
	if errors.Is(err, sql.ErrNoRows) {
		// The singleton row is seeded by migration 000017 and never deleted, so
		// its absence is a broken schema rather than an unset default. Say so.
		return "", fmt.Errorf("system_settings row is missing; the app schema is not initialised")
	}
	if err != nil {
		return "", fmt.Errorf("read default organization: %w", err)
	}
	if !orgID.Valid {
		return "", nil
	}
	return strings.TrimSpace(orgID.String), nil
}
