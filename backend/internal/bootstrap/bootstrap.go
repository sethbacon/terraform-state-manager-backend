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
// Since the Phase 3 close-out it also runs AFTER the reconcile and carries the
// app table's uuids INTO identity, because the direction of truth reversed: the
// old adopt pass that copied identity's uuids app-side is retired, so id
// alignment between the two schemas is now maintained by seeding identity from
// the app rather than the app from identity.
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
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
	"github.com/terraform-state-manager/terraform-state-manager/internal/tenancy"
)

// Run ensures the default organization exists, reconciles TSM's own per-app
// authorization tables, and then seeds the shared identity schema's role
// templates when this app owns that seed. Idempotent; safe to call on every
// startup.
//
// THE RECONCILE IS PART OF THIS FUNCTION, NOT A SECOND CALL BESIDE IT, and
// seedRoleTemplates is a step INSIDE the reconcile rather than a call before it,
// so the app-side definitions land before the membership scan that depends on
// the tables being coherent.
//
// THE IDENTITY-SIDE SEED RUNS LAST, AFTER THE RECONCILE, and that ordering is
// the Phase 3 close-out. It used to run FIRST so the reconcile's adopt pass
// could copy identity's uuids app-side; the adopt pass is retired with the rest
// of the identity.role_templates reads, so the id flow reversed: the app defines
// its roles (minting uuids on a fresh install), and the identity-side seed then
// writes THOSE uuids into identity.role_templates. On every topology where this
// application owns the identity seed, the two schemas therefore keep speaking
// one uuid dialect — which is what keeps the drift comparison's `mismatched`
// kind quiet — without identity ever being read for it.
//
// appDB is the APPLICATION connection, where the per-app tables live. A nil appDB
// skips the reconcile AND this application's own role seed, for callers with no
// app connection to offer; the server always passes one. With no app table to
// take uuids from, the identity-side seed then mints them identity-side, as it
// always did.
func Run(ctx context.Context, identityDB, appDB *sql.DB, seedRoles bool, revocations *repositories.UserTokenRevocationRepository) error {
	if err := ensureDefaultOrg(ctx, identityDB); err != nil {
		return fmt.Errorf("ensure default organization: %w", err)
	}
	if appDB == nil {
		if seedRoles {
			if err := seedSharedRoleTemplates(ctx, identityDB, nil); err != nil {
				return fmt.Errorf("seed shared role templates: %w", err)
			}
		}
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
	channelTable, err := identitynotify.VerifyChannelTable(ctx, appDB)
	if err != nil {
		return fmt.Errorf("verify the notification_channels table: %w", err)
	}
	slog.Info("notification channel table verified", "table", channelTable)

	// AND ITS organization_id COLUMN, which this file's previous note deferred to
	// "the change that first scopes a channel read". That is this change: the
	// #393 Phase 3 flip makes every /notifications/channels read bind
	// `organization_id = ANY($n)`.
	//
	// The deferral was right at the time, and its stated reason had already gone
	// stale before this: it said nothing here passed notify.WithOrgScope, and the
	// delivery path had since started passing it at every Notify call site.
	//
	// WHAT THIS ACTUALLY BUYS, stated precisely, because it is less than it first
	// looks and it is not nothing. A missing column does NOT get a deployment
	// past startup today — tenancy.Backfill runs unconditionally on every boot
	// and UPDATEs organization_id on all nine roots, so it would already die.
	// Three things change:
	//
	//  1. WHEN. This is among the schema assertions at the top of the
	//     app-connection sequence, before authorization reconciliation and role
	//     seeding. This file's own rule is that reconciling against a schema that
	//     is already wrong buys nothing.
	//  2. WHAT IT NAMES. The backfill's failure reads "backfill
	//     notification_channels.organization_id: column does not exist", which
	//     points an operator at the backfill. This one names the partition
	//     migration and the option that requires the column.
	//  3. WHAT IT DOES NOT DEPEND ON. The backfill catches this incidentally,
	//     because notification_channels happens to be in PartitionedTables. The
	//     read predicate binds that column whether or not the backfill still
	//     touches the table, so the assertion belongs next to the reader's
	//     requirement rather than borrowed from an unrelated loop.
	//
	// It sits AFTER VerifyChannelTable because the library's own error text says
	// to call that one first — it is what reports where the table is expected to
	// be.
	if err := identitynotify.VerifyChannelOrganizationColumn(ctx, appDB); err != nil {
		return fmt.Errorf("verify the notification_channels organization column: %w", err)
	}
	slog.Info("notification channel organization column verified",
		"table", channelTable, "column", identitynotify.ChannelOrganizationColumn)

	rep, err := approles.Reconcile(ctx, appDB, identityDB, appRoleDefinitions, retireSessionsOfNarrowedRoles(revocations))
	if err != nil {
		return fmt.Errorf("reconcile per-app authorization tables: %w", err)
	}
	approles.LogReport(rep)

	if seedRoles {
		ids, err := appTemplateIDs(ctx, appDB)
		if err != nil {
			return fmt.Errorf("read this application's role template ids: %w", err)
		}
		if err := seedSharedRoleTemplates(ctx, identityDB, ids); err != nil {
			return fmt.Errorf("seed shared role templates: %w", err)
		}
	}

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
// Keyed by NAME, preserving whatever id the row already carries, so seeding
// cannot orphan an assignment restated from identity.
//
// IT RETURNS THE DEFINITIONS AND WRITES NOTHING (#557). approles.Reconcile
// writes them, at the one point that is right: after it has compared them with
// what the deployment currently records and invalidated the credentials of
// everyone whose authority a narrowing is about to reduce. A definer that wrote
// through a store could narrow a role that the comparison never saw.
func appRoleDefinitions(ctx context.Context) ([]approles.Template, error) {
	seeds := auth.AppRoleTemplates()
	defs := make([]approles.Template, 0, len(seeds))
	for _, rt := range seeds {
		description := rt.Description
		// No id: the write conflicts on name and keeps whatever uuid this
		// deployment already carries, minting a fresh one only for a name it has
		// never held — which no assignment can reference.
		defs = append(defs, approles.Template{
			Name:        rt.Name,
			DisplayName: rt.DisplayName,
			Description: &description,
			Scopes:      rt.Scopes,
			IsSystem:    true,
		})
	}
	return defs, nil
}

// retireSessionsOfNarrowedRoles is TSM's answer to "a role this build defines
// now grants less than the row already in the table" (#557).
//
// IT ENDS SESSIONS, AND DELIBERATELY DOES NOT DESTROY API KEYS.
//
// The two credential families are not in the same position here. A JWT freezes
// its scopes at login and nothing re-derives them, so a holder of a narrowed
// role keeps exercising the removed scopes until their token expires — up to the
// full session lifetime — and the per-JTI denylist cannot help because a
// template narrowing knows no JTIs. The per-user revoke-all watermark (#330) is
// the only instrument that reaches them, and moving it is one set-based write.
//
// An API key is already contained. middleware.authenticateAPIKey caps a key's
// stored scopes by the owner's CURRENT combined scopes on every request and
// strips `admin` unconditionally, so the moment the role narrows, every key its
// holders carry grants less — automatically, with no sweep. Deleting those keys
// would buy hygiene, not containment, and it would buy it at a price this path
// cannot afford: the deletion is irreversible, an API key's secret is shown
// exactly once, and this runs on the startup path before /health answers, inside
// the startup probe's budget. A scope typo in a deployment would become a
// fleet-wide, unrecoverable credential loss discovered from a log line. The
// membership axis destroys keys because there the authority is genuinely gone
// for that principal; here the request-time cap already expresses the reduction.
//
// Ending sessions is not the same trade. It costs the holder a re-login, after
// which they hold exactly the authority the new definition grants.
func retireSessionsOfNarrowedRoles(revocations *repositories.UserTokenRevocationRepository) approles.TemplateAuthorityReducer {
	return func(ctx context.Context, reduced []approles.ReducedTemplate) error {
		if revocations == nil {
			// Refused rather than skipped: a deployment whose sessions cannot be
			// ended must not narrow a role and report a clean boot.
			return fmt.Errorf("no token-revocation repository: cannot end the sessions of holders of %d narrowed role template(s)", len(reduced))
		}
		for _, r := range reduced {
			ended, err := revocations.RevokeAllUserTokensFor(ctx, r.Holders)
			if err != nil {
				return fmt.Errorf("ending sessions for holders of role template %q: %w", r.Name, err)
			}
			slog.Warn("role template narrowed: holders' sessions ended",
				"role", r.Name, "was", r.Was, "now", r.Now,
				"holders", len(r.Holders), "sessions_ended", ended,
				"api_keys", "left in place; capped at request time by the owner's current scopes")
		}
		return nil
	}
}

// appTemplateIDs reads the uuid this application's own role_templates holds for
// each role name, so the identity-side seed can restate the SAME ids.
//
// Read back from the table rather than taken from AppRoleTemplates(), because
// the seed's names carry no ids at all: on an upgraded deployment the table
// holds the uuids the old adopt pass copied from identity years of restarts ago,
// and on a fresh install it holds whatever DefineTemplate just minted. Either
// way, the table is the id space every assignment here is expressed in, and the
// identity copy should speak it too.
func appTemplateIDs(ctx context.Context, appDB *sql.DB) (map[string]string, error) {
	held, err := approles.NewStore(appDB).ListTemplates(ctx)
	if err != nil {
		return nil, err
	}
	ids := make(map[string]string, len(held))
	for name, t := range held {
		ids[name] = t.ID
	}
	return ids, nil
}

// seedSharedRoleTemplates upserts the role -> scope mapping into the SHARED
// identity schema, as it always has, gated by suite.role_seed_owner.
//
// # Why this did not go away when the reads did
//
// TSM no longer reads these rows — not on the primary path since Phase 3b, and
// not anywhere since the Phase 3 close-out retired the residual lookups. It is
// fair to ask why it still writes them. Two reasons, and both expire in Phase 4
// when identity.role_templates is dropped:
//
//	THE ROLLBACK PATH READS THEM. TSM_AUTHZ_ROLE_SOURCE=identity is this phase's
//	rollback, and it puts every role-assignment read back onto this table. A
//	build that stopped seeding it would leave a FRESH standalone deployment with
//	an empty identity.role_templates, so the rollback would resolve every
//	membership to no role and lock everybody out — a rollback lever that works on
//	upgraded deployments and destroys new ones is worse than none.
//
//	THE SIBLING READS THEM. In a coupled deployment the registry still authorizes
//	from this table, and suite.role_seed_owner is still what stops the two apps
//	overwriting each other in it.
//
// # The ids are the app's now
//
// ids maps role name -> the uuid this application's own table holds for it
// (appTemplateIDs); a name with no entry — or a nil map, from the no-app-DB
// caller — is minted identity-side as before. The id is used only on FIRST
// insert of a name: ON CONFLICT (name) cannot rewrite a primary key other rows
// reference, and on the deployments this build upgrades the two ids already
// agree because the old adopt pass copied identity's uuids app-side. This is
// what keeps identity's vestigial organization_members.role_template_id column
// comparable against the app's records (CheckDrift's `mismatched`) without this
// application ever reading identity.role_templates to align anything.
//
// Direct SQL rather than the identity module's repository because that repo
// guards updates to system rows, whereas this seed intentionally owns these
// mappings. scopes is JSONB in the identity schema.
func seedSharedRoleTemplates(ctx context.Context, db *sql.DB, ids map[string]string) error {
	const q = `
		INSERT INTO role_templates (id, name, display_name, description, scopes, is_system, created_at, updated_at)
		VALUES (COALESCE($1::uuid, gen_random_uuid()), $2, $3, $4, $5::jsonb, true, now(), now())
		ON CONFLICT (name) DO UPDATE
		   SET display_name = EXCLUDED.display_name,
		       description  = EXCLUDED.description,
		       scopes       = EXCLUDED.scopes,
		       updated_at   = now()`
	// ALIGNING THE ID ON CONFLICT IS THE POINT, NOT AN EDGE CASE. Identity's
	// own migration 000001 seeds role_templates with gen_random_uuid ids, so on
	// EVERY fresh install the row already exists under a different uuid by the
	// time this runs, the upsert takes the conflict path, and without the
	// alignment below the app and identity hold different ids for the same
	// role forever -- an assignment copied from identity would not resolve
	// here. That is not hypothetical: the fresh-install equivalence tests
	// failed exactly this way until this pass existed.
	//
	// The id can only move while NOTHING references it:
	// organization_members.role_template_id is a real FK with no ON UPDATE
	// action, so updating a referenced parent id is refused by Postgres. The
	// two cases are therefore distinguishable and both are handled honestly:
	//
	//   unreferenced + diverged  -> align (the fresh-install case; identity's
	//                               self-seeded row has no members yet)
	//   referenced   + diverged  -> ERROR naming both ids. This is a real
	//                               collision -- most plausibly another
	//                               application seeded this shared name and
	//                               members were assigned under its id --
	//                               and silently keeping the divergence is
	//                               how a wrong authorization resolves.
	// The align statement is UNCONDITIONAL on references, on purpose: when the
	// diverged row IS referenced, Postgres itself refuses the id change -- the
	// FK on organization_members.role_template_id has no ON UPDATE action --
	// and that refusal is the collision signal, caught and named below. No
	// SELECT, no read-back, no window between checking and changing: the class
	// guard in internal/approles forbids reading identity.role_templates from
	// anywhere but the app-side store, and this path never needs to.
	const align = `
		UPDATE role_templates SET id = $1::uuid, updated_at = now()
		 WHERE name = $2 AND id <> $1::uuid`
	for _, rt := range auth.AppRoleTemplates() {
		scopesJSON, err := json.Marshal(rt.Scopes)
		if err != nil {
			return err
		}
		var id interface{}
		if v, ok := ids[rt.Name]; ok && v != "" {
			id = v
		}
		if _, err := db.ExecContext(ctx, q, id, rt.Name, rt.DisplayName, rt.Description, string(scopesJSON)); err != nil {
			return err
		}
		want, ok := ids[rt.Name]
		if !ok || want == "" {
			continue
		}
		if _, err := db.ExecContext(ctx, align, want, rt.Name); err != nil {
			return fmt.Errorf("role %q: this application holds id %s but identity's row is referenced "+
				"by members and cannot be re-identified. Another application most likely seeded this "+
				"shared name and assignments exist under its id; resolve the seed ownership before "+
				"starting this one: %w", rt.Name, want, err)
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
