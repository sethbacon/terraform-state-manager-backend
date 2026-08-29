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
	// mirror dual-writes the overlay's group-mapping list into TSM's OWN
	// group_mappings table (terraform-suite-identity#206 phase 2, migration
	// 000036). It writes on the APPLICATION connection -- the one the
	// migration created the table on, where role_templates lives -- which is
	// NOT necessarily the connection the authoritative overlay write goes to.
	// Nothing reads the mirrored rows yet.
	mirror *GroupMappingMirror
}

// NewSSOSettingsRepository constructs the DAO. db is the connection the
// overlay is read and written through (the auth handlers pass the identity
// pool); appDB is the APPLICATION connection the group-mapping mirror writes
// on. There is deliberately no mirror-less constructor: a second way to build
// this type would be a legal spelling of "half a dual-write". appDB may be nil
// only in rigs with no application connection at all -- the mirror then
// refuses (absorbed and logged), never panics, and the server always passes a
// real one.
func NewSSOSettingsRepository(db, appDB *sql.DB) *SSOSettingsRepository {
	return &SSOSettingsRepository{db: db, mirror: NewGroupMappingMirror(appDB)}
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
//
// THIS IS THE ONE WRITE SITE for DB-stored group mappings, so the mirror half
// of the phase-2 dual-write (terraform-suite-identity#206, migration 000036)
// runs here on success: TSM's own group_mappings table is made equal to the
// list just stored. One choke point carries all three flavours -- the first
// save inserts the mirror rows, an edit replaces them, and saving an empty
// list deletes them.
//
// The mirror leg runs AFTER the authoritative write has succeeded, and its
// failure is absorbed and logged rather than returned -- the overlay write has
// committed and reads still come from sso_settings, so the caller's request
// succeeded in every observable sense; see groupMappingMirrorFailed for the
// safety argument. The boot reconcile repairs it on the next start. An
// authoritative failure returns BEFORE the mirror runs, so the mirror can
// never hold a list the source refused.
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
	if err != nil {
		return err
	}
	if err := r.mirror.Replace(ctx, s.OIDCGroupMappings); err != nil {
		groupMappingMirrorFailed(ctx, "SSOSettingsRepository.Upsert", err)
	}
	return nil
}
