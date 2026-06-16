// oidc_config_repository.go is the DAO for the runtime OIDC configuration the
// setup wizard writes (migration 000018). The client secret is stored encrypted
// (the handler encrypts before calling Create); this repo never sees plaintext.
package repositories

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"
)

// OIDCConfig is a stored OIDC provider configuration. ClientSecretEncrypted is
// AES-256-GCM ciphertext (internal/crypto); callers decrypt before use.
type OIDCConfig struct {
	IssuerURL             string
	ClientID              string
	ClientSecretEncrypted []byte
	RedirectURL           string
	Scopes                []string
}

// OIDCConfigRepository is the DAO for the oidc_configs table.
type OIDCConfigRepository struct {
	db *sql.DB
}

// NewOIDCConfigRepository constructs the repository over the app/domain DB.
func NewOIDCConfigRepository(db *sql.DB) *OIDCConfigRepository {
	return &OIDCConfigRepository{db: db}
}

// Create deactivates any active config and inserts c as the new active one, in a
// single transaction so there is never zero or two active rows.
func (r *OIDCConfigRepository) Create(ctx context.Context, c OIDCConfig) error {
	scopes := c.Scopes
	if scopes == nil {
		scopes = []string{}
	}
	scopesJSON, err := json.Marshal(scopes)
	if err != nil {
		return err
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		`UPDATE oidc_configs SET is_active = false, updated_at = $1 WHERE is_active = true`, time.Now()); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO oidc_configs (issuer_url, client_id, client_secret_encrypted, redirect_url, scopes, is_active)
		 VALUES ($1, $2, $3, $4, $5::jsonb, true)`,
		c.IssuerURL, c.ClientID, c.ClientSecretEncrypted, c.RedirectURL, string(scopesJSON)); err != nil {
		return err
	}
	return tx.Commit()
}

// GetActiveOIDCConfig returns the active config, or (nil, nil) when none exists.
func (r *OIDCConfigRepository) GetActiveOIDCConfig(ctx context.Context) (*OIDCConfig, error) {
	var c OIDCConfig
	var scopesJSON []byte
	err := r.db.QueryRowContext(ctx,
		`SELECT issuer_url, client_id, client_secret_encrypted, redirect_url, scopes
		   FROM oidc_configs WHERE is_active = true`).
		Scan(&c.IssuerURL, &c.ClientID, &c.ClientSecretEncrypted, &c.RedirectURL, &scopesJSON)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(scopesJSON) > 0 {
		if err := json.Unmarshal(scopesJSON, &c.Scopes); err != nil {
			return nil, err
		}
	}
	return &c, nil
}
