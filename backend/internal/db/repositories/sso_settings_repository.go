package repositories

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

// SSOGroupMapping is one IdP-group -> organization/role mapping row in the
// runtime OIDC group-mapping overlay (mirrors config.OIDCGroupMapping).
type SSOGroupMapping struct {
	Group        string `json:"group"`
	Organization string `json:"organization"`
	Role         string `json:"role"`
}

// SSOSettings is the single-row runtime overlay for the OIDC group-mapping
// configuration. When present it takes precedence over the file config at
// login; the provider itself (issuer, client) stays file-configured.
type SSOSettings struct {
	OIDCGroupClaimName string            `json:"group_claim_name"`
	OIDCDefaultRole    string            `json:"default_role"`
	OIDCGroupMappings  []SSOGroupMapping `json:"group_mappings"`
	UpdatedAt          string            `json:"updated_at"`
}

// SSOSettingsRepository is the DAO for the sso_settings overlay row. The table
// lives in the app schema; both the app connection and the identity connection
// (search_path identity,public) resolve it.
type SSOSettingsRepository struct {
	db *sql.DB
}

func NewSSOSettingsRepository(db *sql.DB) *SSOSettingsRepository {
	return &SSOSettingsRepository{db: db}
}

// Get returns the overlay, or nil when none has been saved yet.
func (r *SSOSettingsRepository) Get(ctx context.Context) (*SSOSettings, error) {
	var s SSOSettings
	var mappings []byte
	err := r.db.QueryRowContext(ctx,
		`SELECT oidc_group_claim_name, oidc_default_role, oidc_group_mappings, updated_at::text
		 FROM sso_settings WHERE id = 1`,
	).Scan(&s.OIDCGroupClaimName, &s.OIDCDefaultRole, &mappings, &s.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(mappings, &s.OIDCGroupMappings); err != nil {
		return nil, err
	}
	return &s, nil
}

// Upsert saves the overlay (insert-or-replace on the singleton row).
func (r *SSOSettingsRepository) Upsert(ctx context.Context, s *SSOSettings) error {
	mappings, err := json.Marshal(s.OIDCGroupMappings)
	if err != nil {
		return err
	}
	_, err = r.db.ExecContext(ctx,
		`INSERT INTO sso_settings (id, oidc_group_claim_name, oidc_default_role, oidc_group_mappings, updated_at)
		 VALUES (1, $1, $2, $3, now())
		 ON CONFLICT (id) DO UPDATE SET
		   oidc_group_claim_name = EXCLUDED.oidc_group_claim_name,
		   oidc_default_role = EXCLUDED.oidc_default_role,
		   oidc_group_mappings = EXCLUDED.oidc_group_mappings,
		   updated_at = now()`,
		s.OIDCGroupClaimName, s.OIDCDefaultRole, mappings,
	)
	return err
}
