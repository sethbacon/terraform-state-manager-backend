// Package bootstrap performs idempotent startup seeding against the shared
// identity schema. The application owns ONLY the role templates (the role→scope
// mapping); the single-tenant default organization is ensured here as a
// bootstrap convenience. Everything else (users, memberships, tokens, OIDC
// config, audit) is owned by the terraform-suite-identity module.
//
// All statements run against the identity-schema connection (search_path =
// identity,public) so unqualified table names resolve to the identity schema.
package bootstrap

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/terraform-state-manager/terraform-state-manager/internal/approles"
	"github.com/terraform-state-manager/terraform-state-manager/internal/auth"
)

// Run ensures the default organization exists and, when seedRoles is true, seeds
// the app-owned role templates. Idempotent; safe to call on every startup.
//
// seedRoles is gated by suite.role_seed_owner: when two apps share one identity
// database, only the designated owner seeds role templates, so the apps don't
// overwrite each other's role scopes. The default organization is always ensured
// — it is a single-tenant app concern, not a cross-app collision.
// THE RECONCILE IS PART OF THIS FUNCTION, NOT A SECOND CALL BESIDE IT.
// approles.Reconcile copies "what TSM resolves today" out of the identity schema
// into TSM's own role_templates and organization_member_roles
// (sethbacon/terraform-suite-identity#206, Phase 3a). What TSM resolves today is
// the OUTPUT of the seed above, so the two are ordered, and a caller who ran only
// the first would leave the mirror describing the previous boot's roles. A
// separate exported step is a step somebody can forget; folding it in makes the
// ordering structural, the same way scim.deprovisionUser makes its membership
// strip and its credential sweep inseparable.
//
// appDB is the APPLICATION connection, where the per-app tables live. A nil
// appDB skips the reconcile, for callers with no app connection to offer; the
// server always passes one.
func Run(ctx context.Context, identityDB, appDB *sql.DB, seedRoles bool) error {
	if seedRoles {
		if err := seedRoleTemplates(ctx, identityDB); err != nil {
			return fmt.Errorf("seed role templates: %w", err)
		}
	}
	if err := ensureDefaultOrg(ctx, identityDB); err != nil {
		return fmt.Errorf("ensure default organization: %w", err)
	}
	if appDB == nil {
		return nil
	}
	rep, err := approles.Reconcile(ctx, appDB, identityDB)
	if err != nil {
		return fmt.Errorf("reconcile per-app authorization tables: %w", err)
	}
	approles.LogReport(rep)
	return nil
}

// seedRoleTemplates upserts the application's role→scope mapping by name. We use
// direct SQL (not the identity RoleTemplateRepository) because that repo guards
// updates to system rows, whereas the app intentionally owns these mappings.
// scopes is JSONB in the identity schema.
func seedRoleTemplates(ctx context.Context, db *sql.DB) error {
	const q = `
		INSERT INTO role_templates (id, name, display_name, description, scopes, is_system, created_at, updated_at)
		VALUES (gen_random_uuid(), $1, $2, $3, $4::jsonb, true, now(), now())
		ON CONFLICT (name) DO UPDATE
		   SET display_name = EXCLUDED.display_name,
		       description  = EXCLUDED.description,
		       scopes       = EXCLUDED.scopes,
		       updated_at   = now()`
	for _, rt := range auth.AppRoleTemplates() {
		scopesJSON, err := json.Marshal(rt.Scopes)
		if err != nil {
			return err
		}
		if _, err := db.ExecContext(ctx, q, rt.Name, rt.DisplayName, rt.Description, string(scopesJSON)); err != nil {
			return err
		}
	}
	return nil
}

// ensureDefaultOrg creates the single-tenant "default" organization if absent.
func ensureDefaultOrg(ctx context.Context, db *sql.DB) error {
	const q = `
		INSERT INTO organizations (id, name, display_name, created_at, updated_at)
		SELECT gen_random_uuid(), 'default', 'Default', now(), now()
		WHERE NOT EXISTS (SELECT 1 FROM organizations WHERE name = 'default')`
	_, err := db.ExecContext(ctx, q)
	return err
}
