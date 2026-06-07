package repositories

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"

	identitymodels "github.com/sethbacon/terraform-suite-identity/identity/models"
	identitystore "github.com/sethbacon/terraform-suite-identity/identity/store"
)

// OIDCConfigRepository wraps the shared identity OIDC-config CRUD repository and
// adds TSM's app-owned setup-wizard methods (setup completion state and the
// one-time setup-token hash, stored in the system_settings key-value table).
//
// Per the suite identity boundary, the shared module owns OIDC-config data
// access; each app owns its own setup wizard. The embedded module repository
// supplies GetActiveOIDCConfig / GetOIDCConfig / ListOIDCConfigs /
// CreateOIDCConfig / DeleteOIDCConfig / … via promotion; the methods below are
// TSM-specific.
type OIDCConfigRepository struct {
	*identitystore.OIDCConfigRepository
	db *sqlx.DB
}

// NewOIDCConfigRepository constructs an OIDCConfigRepository over the given connection.
func NewOIDCConfigRepository(db *sqlx.DB) *OIDCConfigRepository {
	return &OIDCConfigRepository{
		OIDCConfigRepository: identitystore.NewOIDCConfigRepository(db),
		db:                   db,
	}
}

// SaveOIDCConfig deactivates any existing active configuration and inserts a new
// active one. Used by the setup wizard.
func (r *OIDCConfigRepository) SaveOIDCConfig(ctx context.Context, cfg *identitymodels.OIDCConfig) error {
	if _, err := r.db.ExecContext(ctx, "UPDATE oidc_config SET is_active = false WHERE is_active = true"); err != nil {
		return fmt.Errorf("failed to deactivate existing OIDC configs: %w", err)
	}

	scopes := cfg.Scopes
	if len(scopes) == 0 {
		scopes = json.RawMessage(`["openid","email","profile"]`)
	}
	name := cfg.Name
	if name == "" {
		name = "default"
	}
	providerType := cfg.ProviderType
	if providerType == "" {
		providerType = "oidc"
	}

	if _, err := r.db.ExecContext(ctx,
		`INSERT INTO oidc_config
		    (id, name, provider_type, issuer_url, client_id, client_secret_encrypted, redirect_url, scopes, is_active, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, true, NOW(), NOW())`,
		uuid.New(), name, providerType, cfg.IssuerURL, cfg.ClientID, cfg.ClientSecretEncrypted, cfg.RedirectURL, scopes,
	); err != nil {
		return fmt.Errorf("failed to save OIDC config: %w", err)
	}
	return nil
}

// IsSetupCompleted reports whether the first-run setup wizard has been completed.
func (r *OIDCConfigRepository) IsSetupCompleted(ctx context.Context) (bool, error) {
	var value string
	err := r.db.GetContext(ctx, &value, "SELECT value FROM system_settings WHERE key = 'setup_completed'")
	if err != nil {
		return false, nil // Default to not completed.
	}
	return value == "true", nil
}

// SetSetupCompleted marks the setup wizard as completed.
func (r *OIDCConfigRepository) SetSetupCompleted(ctx context.Context) error {
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO system_settings (key, value, updated_at) VALUES ('setup_completed', 'true', NOW())
		 ON CONFLICT (key) DO UPDATE SET value = 'true', updated_at = NOW()`)
	if err != nil {
		return fmt.Errorf("failed to set setup completed: %w", err)
	}
	return nil
}

// GetSetupTokenHash returns the stored bcrypt hash of the one-time setup token.
func (r *OIDCConfigRepository) GetSetupTokenHash(ctx context.Context) (string, error) {
	var value string
	err := r.db.GetContext(ctx, &value, "SELECT value FROM system_settings WHERE key = 'setup_token_hash'")
	if err != nil {
		return "", nil // No token hash exists.
	}
	return value, nil
}

// SetSetupTokenHash stores the bcrypt hash of the one-time setup token.
func (r *OIDCConfigRepository) SetSetupTokenHash(ctx context.Context, hash string) error {
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO system_settings (key, value, updated_at) VALUES ('setup_token_hash', $1, NOW())
		 ON CONFLICT (key) DO UPDATE SET value = $1, updated_at = NOW()`,
		hash,
	)
	if err != nil {
		return fmt.Errorf("failed to set setup token hash: %w", err)
	}
	return nil
}
