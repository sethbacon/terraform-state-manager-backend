// Package bootstrap performs idempotent startup seeding for the two schemas TSM
// touches: the shared identity schema (terraform-suite-identity), and TSM's own
// per-app authorization tables.
//
// Since Phase 3b of sethbacon/terraform-suite-identity#206, THIS APPLICATION'S
// ROLE DEFINITIONS ARE ITS OWN. seedRoleTemplates writes them into TSM's
// role_templates on the APPLICATION connection, unconditionally, because a
// per-app table has no sibling to collide with. The identity-side seed is still
// performed, still gated by suite.role_seed_owner, and is no longer what TSM
// authorizes against — see seedSharedRoleTemplates for the two reasons it stays.
//
// The identity statements run against the identity-schema connection
// (search_path = identity,public) so unqualified table names resolve there.
package bootstrap

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"

	identitynotify "github.com/sethbacon/terraform-suite-identity/identity/notify"

	"github.com/terraform-state-manager/terraform-state-manager/internal/approles"
	"github.com/terraform-state-manager/terraform-state-manager/internal/auth"
	"github.com/terraform-state-manager/terraform-state-manager/internal/tenancy"
)

// Run ensures the default organization exists, seeds the shared identity schema's
// role templates when this app owns that seed, and reconciles TSM's own per-app
// authorization tables. Idempotent; safe to call on every startup.
//
// THE RECONCILE IS PART OF THIS FUNCTION, NOT A SECOND CALL BESIDE IT, and
// seedRoleTemplates is a step INSIDE the reconcile rather than a call before it.
// approles.Reconcile has to adopt identity's role templates — and therefore
// identity's uuids, so a restated assignment resolves — BEFORE this build's own
// definitions are written over those rows by name. Calling the seed here, either
// side of Reconcile, would be the wrong order in one of the two directions and
// would silently cost a fresh install either its own role scopes or its
// assignments. Handing the seed to Reconcile makes that ordering structural, the
// same way folding the reconcile into this function makes its ordering after the
// identity seed structural.
//
// appDB is the APPLICATION connection, where the per-app tables live. A nil appDB
// skips the reconcile AND this application's own role seed, for callers with no
// app connection to offer; the server always passes one.
func Run(ctx context.Context, identityDB, appDB *sql.DB, seedRoles bool) error {
	if seedRoles {
		if err := seedSharedRoleTemplates(ctx, identityDB); err != nil {
			return fmt.Errorf("seed shared role templates: %w", err)
		}
	}
	if err := ensureDefaultOrg(ctx, identityDB); err != nil {
		return fmt.Errorf("ensure default organization: %w", err)
	}
	if appDB == nil {
		return nil
	}

	// The shape assertion identity/notify ships so that a schema mismatch is a
	// STARTUP failure naming the migration, rather than a runtime SQL error on
	// the first notification — or, worse, a silently empty channel list, which
	// reads as "nobody configured any" instead of "this deployment is broken".
	//
	// notification_channels is OUR table (000009); the module only supplies the
	// canonical DDL and this check. So nothing but this call stands between a
	// drifted local schema and a failure discovered by a customer.
	//
	// FIRST among the app-connection steps, deliberately: it is the cheapest and
	// the only one whose failure means "do not start at all". Reconciling
	// authorization or backfilling a partition against a schema that is already
	// wrong buys nothing.
	//
	// VerifyChannelOrganizationColumn is NOT called here, and that is a decision
	// rather than an omission. It asserts the organization_id column that #393's
	// migration 000033 added, and nothing in this repository passes
	// notify.WithOrgScope yet. Failing a boot over a column no statement reads
	// would refuse to start a deployment for a capability it is not using. Add
	// the call in the change that first scopes a channel read — #393 Phase 3 —
	// where a missing column becomes a real fault.
	channelTable, err := identitynotify.VerifyChannelTable(ctx, appDB)
	if err != nil {
		return fmt.Errorf("verify the notification_channels table: %w", err)
	}
	slog.Info("notification channel table verified", "table", channelTable)

	rep, err := approles.Reconcile(ctx, appDB, identityDB, seedRoleTemplates)
	if err != nil {
		return fmt.Errorf("reconcile per-app authorization tables: %w", err)
	}
	approles.LogReport(rep)

	// Phase 1 of #393: give the app connection the default organization's id, and
	// stamp the rows that predate migration 000033's organization_id column.
	//
	// THIS CANNOT BE A MIGRATION, and 000033's header gives the three independent
	// reasons. The one this call site embodies is ORDERING: cmd/server/main.go
	// runs the app migrations at line 220 and reaches this function at line 275,
	// so at migration time the organizations table may not exist yet and the
	// default row certainly does not. Here it does, because ensureDefaultOrg has
	// just run above.
	//
	// LAST, AFTER THE RECONCILE, not because it depends on it but because it is
	// the step whose failure is least damaging to defer. The reconcile decides
	// authorization; this writes a column nothing reads until Phase 2.
	orgID, err := defaultOrgID(ctx, identityDB)
	if err != nil {
		return fmt.Errorf("resolve the default organization id: %w", err)
	}
	if err := tenancy.Backfill(ctx, appDB, orgID); err != nil {
		return fmt.Errorf("backfill the organization partition: %w", err)
	}
	return nil
}

// defaultOrgID reads back the organization ensureDefaultOrg has just guaranteed.
//
// A separate query rather than a RETURNING clause on ensureDefaultOrg's INSERT,
// because that INSERT is conditional (`WHERE NOT EXISTS`) and returns NO ROW on
// the overwhelmingly common path where the organization already exists. A
// RETURNING there would hand back an id on first boot and nothing on every boot
// after, which is the shape of bug that works in every test that starts from an
// empty database.
//
// On the IDENTITY connection, whose search_path resolves `organizations`
// (cmd/server/main.go opens it with `identity,public`). The app connection
// cannot run this — 000032's routing pre-check exists to guarantee it cannot —
// which is exactly why the id is fetched here and passed across.
func defaultOrgID(ctx context.Context, db *sql.DB) (string, error) {
	var id string
	if err := db.QueryRowContext(ctx,
		`SELECT id::text FROM organizations WHERE name = 'default'`).Scan(&id); err != nil {
		return "", err
	}
	return id, nil
}

// seedRoleTemplates writes this build's role -> scope mapping into THIS
// APPLICATION's own role_templates.
//
// UNCONDITIONAL, and that is the point of Phase 3b. The old gate existed because
// identity.role_templates.name is globally UNIQUE and two apps seeding the shared
// table overwrote each other's scopes on every restart. This table is TSM's, its
// name uniqueness is per-application, and there is no sibling writing it — so
// there is nothing left to arbitrate and no configuration that should be able to
// leave this application's roles undefined.
//
// Keyed by NAME, preserving whatever id the row already carries (see
// Store.DefineTemplate), so seeding cannot orphan an assignment restated from
// identity. It is handed to approles.Reconcile rather than called directly,
// because it is only correct AFTER the adopt pass.
func seedRoleTemplates(ctx context.Context, store *approles.Store) error {
	for _, rt := range auth.AppRoleTemplates() {
		description := rt.Description
		// No id: DefineTemplate keeps the adopted one on conflict and mints a
		// fresh uuid only for a name identity does not have — which no assignment
		// can reference.
		if err := store.DefineTemplate(ctx, approles.Template{
			Name:        rt.Name,
			DisplayName: rt.DisplayName,
			Description: &description,
			Scopes:      rt.Scopes,
			IsSystem:    true,
		}); err != nil {
			return err
		}
	}
	return nil
}

// seedSharedRoleTemplates upserts the role -> scope mapping into the SHARED
// identity schema, as it always has, gated by suite.role_seed_owner.
//
// # Why this did not go away with Phase 3b
//
// TSM no longer reads these rows, so it is fair to ask why it still writes them.
// Two reasons, and both expire in Phase 4 when identity.role_templates is dropped:
//
//	THE ROLLBACK PATH READS THEM. TSM_AUTHZ_ROLE_SOURCE=identity is this phase's
//	rollback, and it puts every authorization decision back onto this table. A
//	build that stopped seeding it would leave a FRESH standalone deployment with
//	an empty identity.role_templates, so the rollback would resolve every
//	membership to no role and lock everybody out — a rollback lever that works on
//	upgraded deployments and destroys new ones is worse than none.
//
//	THE SIBLING READS THEM. In a coupled deployment the registry still authorizes
//	from this table, and suite.role_seed_owner is still what stops the two apps
//	overwriting each other in it.
//
// Direct SQL rather than the identity RoleTemplateRepository because that repo
// guards updates to system rows, whereas this seed intentionally owns these
// mappings. scopes is JSONB in the identity schema.
func seedSharedRoleTemplates(ctx context.Context, db *sql.DB) error {
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
