package approles

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"sort"

	idstore "github.com/sethbacon/terraform-suite-identity/identity/store"

	"github.com/terraform-state-manager/terraform-state-manager/internal/auth"
)

// membershipPage bounds one pass of the assignment scan.
//
// Keyset pagination over (organization_id, user_id) rather than one unbounded
// statement, so a deployment with a large membership table does bounded work per
// round trip. The key is a primary key whose columns never change, which is what
// makes paging SAFE HERE despite the sweep that follows: a row can be missed by
// the scan only if its key moves, and these keys do not. A row inserted ahead of
// the cursor mid-scan is missed, but it was written either by this application's
// mirror (mirrored_at is newer than the generation, so the sweep spares it) or by
// the sibling app (absent from the mirror, so there is nothing to sweep).
const membershipPage = 1000

// Report is what one reconcile did, for the startup line and for tests.
type Report struct {
	// TemplatesCopied is the number of identity role templates restated in TSM's
	// own role_templates.
	TemplatesCopied int
	// AssignmentsRestated is the number of (organization_id, user_id) rows read
	// from identity and written to TSM's own organization_member_roles.
	AssignmentsRestated int
	// StaleRemoved is the number of mirrored assignments deleted because
	// identity no longer has them.
	StaleRemoved int64
	// DivergentTemplates names the role templates whose scopes, as TSM resolves
	// them today, are NOT the scopes this build defines for that name. See
	// TemplateDivergenceRemedy.
	DivergentTemplates []string
	// Templates and Assignments are where the two unqualified names actually
	// resolved on the app connection.
	Templates, Assignments string
}

// Reconcile rebuilds TSM's per-app authorization tables from what TSM resolves
// today, and is both the Phase 3a backfill and its standing repair.
//
// # Why the backfill is code and not SQL in the migration
//
// The obvious backfill — INSERT ... SELECT from identity.organization_members —
// cannot be written. Migration 000031 runs on the APP connection, and identity
// may be in a separate database (TSM_IDENTITY_DATABASE_*) where that SELECT has
// nothing to name. The copy has to cross two connections, so it is a read here
// and a write there.
//
// # What "what TSM resolves today" means, exactly
//
// Not auth.AppRoleTemplates(). TSM's effective role -> scope mapping is whatever
// identity.role_templates HOLDS at the moment a session's scopes are derived —
// that is the table GetUserCombinedScopes joins. In a standalone deployment TSM
// seeded those rows itself moments earlier (internal/bootstrap), so the two
// coincide. In a coupled deployment (TSM_SUITE_ROLE_SEED_OWNER=registry) they do
// NOT: the rows hold the SIBLING's scopes, and that is what TSM authorizes
// against right now. Copying identity's rows verbatim therefore captures the
// effective mapping in both cases, and copying auth.AppRoleTemplates() would
// capture it in only one — silently CHANGING a coupled deployment's
// authorization the moment reads move. Phase 3a is required to change nothing.
//
// The divergence is not swallowed, though: DivergentTemplates names every role
// whose effective scopes are not this build's, so an operator can see that this
// deployment's `editor` is not TSM's `editor` before the phase that makes it
// permanent. See TemplateDivergenceRemedy.
//
// # Ordering
//
// Verify, then templates, then assignments, then the sweep. Templates first
// because organization_member_roles.role_template_id has a real foreign key to
// them; the sweep last because it deletes everything the assignment pass did not
// restate, and a pass that did not COMPLETE must not be allowed to conclude that
// the rest is stale. Any error before the sweep returns without sweeping.
//
// # Idempotent, and safe to run on every boot
//
// It is run on every boot — that is what makes a mirror write that failed, a row
// removed by CASCADE, and a row written by the sibling registry converge instead
// of accumulating. Running it twice changes nothing the first run did not
// already do.
func Reconcile(ctx context.Context, appDB, identityDB *sql.DB) (Report, error) {
	var rep Report
	if appDB == nil {
		return rep, fmt.Errorf("%w: no application database connection", ErrMisrouted)
	}
	if identityDB == nil {
		return rep, errors.New("approles: no identity database connection to reconcile from")
	}

	store := NewStore(appDB)
	templatesName, assignmentsName, err := store.Verify(ctx)
	if err != nil {
		return rep, err
	}
	rep.Templates, rep.Assignments = templatesName, assignmentsName

	generation, err := store.Generation(ctx)
	if err != nil {
		return rep, err
	}

	if err := reconcileTemplates(ctx, store, identityDB, &rep); err != nil {
		return rep, err
	}
	if err := reconcileAssignments(ctx, store, identityDB, &rep); err != nil {
		return rep, err
	}

	removed, err := store.SweepStaleAssignments(ctx, generation)
	if err != nil {
		return rep, err
	}
	rep.StaleRemoved = removed
	return rep, nil
}

// reconcileTemplates copies every identity role template into TSM's own table,
// preserving identity's uuid so organization_member_roles can store the same
// role_template_id identity's membership row stores.
//
// Copies are UPSERTS and there is no delete pass. A template that disappeared
// from identity is left standing here rather than dropped, because dropping it
// would SET NULL the assignments referencing it — turning a template somebody
// removed upstream into a silent loss of every role that used it. It is inert
// (nothing points at it once the assignment pass runs) and visible to the
// operator in the table.
func reconcileTemplates(ctx context.Context, store *Store, identityDB *sql.DB, rep *Report) error {
	templates, err := idstore.NewRoleTemplateRepository(identityDB).ListRoleTemplates(ctx)
	if err != nil {
		return fmt.Errorf("approles: reading identity role templates: %w", err)
	}

	expected := expectedScopes()
	divergent := make([]string, 0)
	for _, rt := range templates {
		if rt == nil {
			continue
		}
		t := templateFromIdentity(rt.ID.String(), rt.Name, rt.DisplayName, rt.Description, rt.Scopes, rt.IsSystem)
		// A name this deployment already has under a DIFFERENT id: identity
		// dropped the template and recreated it. Release the name before the
		// insert, or the unique index rejects it and the reconcile wedges on a
		// row nobody can reach.
		if err := store.RepointTemplateName(ctx, t.Name, t.ID); err != nil {
			return err
		}
		if err := store.UpsertTemplate(ctx, t); err != nil {
			return err
		}
		rep.TemplatesCopied++

		if want, ours := expected[t.Name]; ours && !sameScopeSet(want, t.Scopes) {
			divergent = append(divergent, t.Name)
		}
	}
	sort.Strings(divergent)
	rep.DivergentTemplates = divergent
	return nil
}

// reconcileAssignments restates every identity membership as a row of TSM's own
// organization_member_roles.
//
// The read is raw SQL against the identity connection because the shared store
// has no "every membership" accessor — GetUserMemberships is per principal, and
// calling it once per user would be a query per user to answer a question that
// is one scan. It is a READ of a table this application already reads through
// that store on every login; nothing here writes identity.
func reconcileAssignments(ctx context.Context, store *Store, identityDB *sql.DB, rep *Report) error {
	const q = `
		SELECT organization_id, user_id, role_template_id
		FROM organization_members
		WHERE (organization_id, user_id) > ($1, $2)
		ORDER BY organization_id, user_id
		LIMIT $3`

	// The empty uuid is below every real one, so it opens the keyset scan.
	lastOrg, lastUser := "00000000-0000-0000-0000-000000000000", "00000000-0000-0000-0000-000000000000"
	for {
		rows, err := identityDB.QueryContext(ctx, q, lastOrg, lastUser, membershipPage)
		if err != nil {
			return fmt.Errorf("approles: reading identity memberships: %w", err)
		}
		page := 0
		for rows.Next() {
			var orgID, userID string
			var roleTemplateID sql.NullString
			if err := rows.Scan(&orgID, &userID, &roleTemplateID); err != nil {
				_ = rows.Close()
				return fmt.Errorf("approles: reading an identity membership: %w", err)
			}
			var role *string
			if roleTemplateID.Valid {
				v := roleTemplateID.String
				role = &v
			}
			if err := store.SetRole(ctx, orgID, userID, role); err != nil {
				_ = rows.Close()
				return err
			}
			lastOrg, lastUser = orgID, userID
			page++
			rep.AssignmentsRestated++
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			// Returned, NOT swallowed. The caller must not reach the sweep on a
			// partial pass: everything this scan did not restate would look
			// stale, and a transient identity fault would empty the mirror.
			return fmt.Errorf("approles: reading identity memberships: %w", err)
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("approles: reading identity memberships: %w", err)
		}
		if page < membershipPage {
			return nil
		}
	}
}

// expectedScopes is this build's own role -> scope mapping, keyed by name, for
// the divergence report only. It is deliberately NOT what gets written.
func expectedScopes() map[string][]string {
	out := make(map[string][]string)
	for _, rt := range auth.AppRoleTemplates() {
		out[rt.Name] = rt.Scopes
	}
	return out
}

// sameScopeSet compares two scope lists as SETS. Order and duplicates are not
// meaningful in a role template's scopes — HasScope iterates — so comparing them
// as sequences would report divergence for two identical roles.
func sameScopeSet(a, b []string) bool {
	seen := make(map[string]struct{}, len(a))
	for _, s := range a {
		seen[s] = struct{}{}
	}
	other := make(map[string]struct{}, len(b))
	for _, s := range b {
		other[s] = struct{}{}
	}
	if len(seen) != len(other) {
		return false
	}
	for s := range seen {
		if _, ok := other[s]; !ok {
			return false
		}
	}
	return true
}

// TemplateDivergenceRemedy is what an operator does about Report.DivergentTemplates.
//
// It is a WARNING and not a startup failure, on purpose. A divergent template is
// not corruption: it is this deployment authorizing against the sibling app's
// definition of a role name, which is the pre-existing condition
// TSM_SUITE_ROLE_SEED_OWNER was invented to manage and which this phase is
// required not to change. Failing the boot would take a working deployment down
// to report a state it has been running in for months.
const TemplateDivergenceRemedy = "this deployment's role scopes come from the sibling app (TSM_SUITE_ROLE_SEED_OWNER is not 'self' or 'tsm'), " +
	"so these roles do not mean here what this build defines them to mean. They have been mirrored AS THEY ARE, because this phase must not change " +
	"authorization. To adopt this build's definitions instead, set suite.role_seed_owner to 'self' and restart — but only once the sibling app no longer " +
	"reads them, since that setting is what stops the two apps overwriting each other in the shared identity schema."

// DriftQuery reports mirrored role assignments that no longer agree with
// identity, for an operator who needs to see divergence between reconciles.
//
// SAME-DATABASE ONLY. It names both schemas in one statement, so it runs as
// written only on the default topology where identity shares the app database.
// With TSM_IDENTITY_DATABASE_* set there is no statement that can join them:
// dump `SELECT organization_id, user_id, role_template_id FROM identity.organization_members
// ORDER BY 1,2` and `SELECT organization_id, user_id, role_template_id FROM
// organization_member_roles ORDER BY 1,2` from their respective connections and
// diff the two.
//
// Three kinds of row come back:
//
//	missing    — identity has the membership, the mirror does not
//	stale      — the mirror has an assignment identity no longer has
//	mismatched — both have it, with different roles
//
// WHAT AN OPERATOR DOES: nothing urgent. In Phase 3a no authorization decision
// reads the mirrored table, so drift denies nobody and grants nobody; restarting
// the backend runs Reconcile, which restates every assignment and sweeps what
// identity no longer has. Drift that SURVIVES a restart is the report worth
// acting on — it means the reconcile is failing rather than that a single write
// slipped, and the startup log will say why.
const DriftQuery = `
SELECT 'missing' AS kind, m.organization_id, m.user_id, m.role_template_id AS identity_role, NULL::uuid AS app_role
  FROM identity.organization_members m
  LEFT JOIN organization_member_roles r
    ON r.organization_id = m.organization_id AND r.user_id = m.user_id
 WHERE r.organization_id IS NULL
UNION ALL
SELECT 'stale', r.organization_id, r.user_id, NULL::uuid, r.role_template_id
  FROM organization_member_roles r
  LEFT JOIN identity.organization_members m
    ON m.organization_id = r.organization_id AND m.user_id = r.user_id
 WHERE m.organization_id IS NULL
UNION ALL
SELECT 'mismatched', m.organization_id, m.user_id, m.role_template_id, r.role_template_id
  FROM identity.organization_members m
  JOIN organization_member_roles r
    ON r.organization_id = m.organization_id AND r.user_id = m.user_id
 WHERE m.role_template_id IS DISTINCT FROM r.role_template_id
ORDER BY 1, 2, 3`

// LogReport writes the startup line for one reconcile, including where the two
// unqualified table names actually resolved.
//
// The resolved names are logged for the same reason the platform-admin carrier
// logs its own: both are unqualified and placed by search_path, and a mirror that
// has been writing into the shared identity table is indistinguishable from a
// working one in every other observable.
func LogReport(rep Report) {
	slog.Info("per-app authorization tables reconciled",
		"role_templates", rep.Templates, "organization_member_roles", rep.Assignments,
		"templates_copied", rep.TemplatesCopied,
		"assignments_restated", rep.AssignmentsRestated,
		"stale_removed", rep.StaleRemoved)
	if len(rep.DivergentTemplates) > 0 {
		slog.Warn("role templates do not carry this build's scopes",
			"roles", rep.DivergentTemplates, "remedy", TemplateDivergenceRemedy)
	}
}
