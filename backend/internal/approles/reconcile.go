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
	// TemplatesAdopted is the number of identity role templates this application
	// did not already hold and has taken a copy of. Adopted, not overwritten —
	// see reconcileTemplates.
	TemplatesAdopted int
	// TemplatesDefined is the number of role definitions this build wrote as its
	// own (the app-side seed).
	TemplatesDefined int
	// AssignmentsRestated is the number of (organization_id, user_id) rows read
	// from identity and written to TSM's own organization_member_roles.
	AssignmentsRestated int
	// StaleRemoved is the number of mirrored assignments deleted because
	// identity no longer has them.
	StaleRemoved int64
	// ForeignTemplates names the role templates this application holds but does
	// NOT define — adopted from identity, and therefore meaning here whatever the
	// application that seeded them there decided. See ForeignTemplateRemedy.
	ForeignTemplates []string
	// PendingRepairs is what the two sides disagreed about BEFORE this reconcile
	// changed anything. In Phase 3a that was a curiosity. Now it is the list of
	// authorization records this run is about to rewrite — see Reconcile.
	PendingRepairs DriftResult
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
// cannot be written. Migration 000032 runs on the APP connection, and identity
// may be in a separate database (TSM_IDENTITY_DATABASE_*) where that SELECT has
// nothing to name. The copy has to cross two connections, so it is a read here
// and a write there.
//
// # Where the effective role -> scope mapping comes from now
//
// From auth.AppRoleTemplates(), written by defineOwn — and that is the change
// Phase 3b makes here. In Phase 3a the mapping was whatever identity.role_templates
// HELD, because that was the table GetUserCombinedScopes joined; copying it
// verbatim was the only way to change nothing. A coupled deployment
// (TSM_SUITE_ROLE_SEED_OWNER=registry) has been authorizing against the SIBLING's
// definition of every role name, and Phase 3a's report named those roles so an
// operator could see it "before the phase that makes it permanent". This is that
// phase, in the other direction: this application now defines its own roles, and a
// coupled deployment's `editor` becomes TSM's `editor` at the first boot on this
// build. That is the intended correction, it is the only authorization change this
// phase makes on purpose, and it affects ONLY deployments that set
// suite.role_seed_owner away from its default.
//
// Roles this build does not define are still adopted as they are, and named:
// ForeignTemplates lists every role whose meaning this application does not own.
// See ForeignTemplateRemedy.
//
// # Ordering
//
// Verify, then the pending-repair report, then ADOPT identity's templates, then
// DEFINE this build's own, then assignments, then the sweep.
//
// Templates before assignments because organization_member_roles.role_template_id
// has a real foreign key to them. Adopt before define because the two answer
// different questions and only one order works: adopting brings identity's uuid
// across so an assignment restated from identity resolves here, and defining then
// replaces that row's SCOPES by name while leaving its id alone. Reversed, a
// fresh install would mint its own uuid for `editor`, adoption would then find
// identity's `editor` under a different uuid, RepointTemplateName would delete
// the row just seeded, and this build's scopes would be silently replaced by
// identity's — which is the failure this phase exists to end, reintroduced by an
// ordering nobody would look at twice.
//
// The sweep is last because it deletes everything the assignment pass did not
// restate, and a pass that did not COMPLETE must not be allowed to conclude that
// the rest is stale. Any error before the sweep returns without sweeping.
//
// # What changed now that reads depend on this
//
// In Phase 3a this function rebuilt a table nothing read. Two of the things it
// did were free then and are not free now:
//
//   - IT OVERWROTE ROLE DEFINITIONS from identity, every boot. That was correct
//     while identity's definitions WERE the effective ones. Now they are not, and
//     an overwrite would let identity — in a coupled deployment, the sibling
//     registry — silently redefine what a TSM role grants, once per restart, on
//     the table that decides authorization. So identity may still SUPPLY a
//     definition this deployment has never seen (Store.AdoptTemplate) and may no
//     longer REDEFINE one.
//
//   - IT REPAIRED ASSIGNMENTS SILENTLY. Restating a membership was a no-op nobody
//     could observe. Now every repair is an authorization change made by a batch
//     job at boot: a `missing` row it fixes GRANTS access, a `stale` row it sweeps
//     REVOKES it. So the comparison runs FIRST and its result is carried on the
//     report and logged — the reconcile now says what it is about to change,
//     before it changes it, and a boot that rewrites nothing is visibly different
//     from one that rewrote four hundred principals' authority.
//
// What did NOT change is that it still repairs. Turning it into a reporter was
// considered and is wrong: identity.organization_members rows disappear by ON
// DELETE CASCADE when an organization or a user is deleted, with no statement
// this application ever sees, and the sweep is the ONLY thing that withdraws the
// matching authority here. A report-only reconcile would leave a deleted user's
// role standing in the table that now decides what they may do.
//
// # defineOwn
//
// The app-side seed (bootstrap.seedRoleTemplates), supplied by the application
// and MANDATORY. A nil one is refused rather than skipped: a reconcile that
// adopted identity's definitions and never wrote this build's would leave the
// mirror carrying identity's scopes, which is the Phase 3a meaning of these rows
// and the wrong answer for a build that reads them. The same shape, and the same
// reason, as AuthorityReducer in members.go.
//
// # Idempotent, and safe to run on every boot
//
// Running it twice changes nothing the first run did not already do.
func Reconcile(ctx context.Context, appDB, identityDB *sql.DB, defineOwn TemplateDefiner) (Report, error) {
	var rep Report
	if appDB == nil {
		return rep, fmt.Errorf("%w: no application database connection", ErrMisrouted)
	}
	if identityDB == nil {
		return rep, errors.New("approles: no identity database connection to reconcile from")
	}
	if defineOwn == nil {
		return rep, errors.New("approles: no role-template definer supplied: the reconcile would leave this application's " +
			"role definitions as identity wrote them, which is not what this build grants for those names")
	}

	store := NewStore(appDB)
	templatesName, assignmentsName, err := store.Verify(ctx)
	if err != nil {
		return rep, err
	}
	rep.Templates, rep.Assignments = templatesName, assignmentsName

	// BEFORE ANYTHING IS WRITTEN. This is the set of authorization records the
	// passes below are about to rewrite; taken afterwards it would always be
	// empty and would report only that the reconcile ran.
	if rep.PendingRepairs, err = CheckDrift(ctx, appDB, identityDB); err != nil {
		return rep, err
	}

	generation, err := store.Generation(ctx)
	if err != nil {
		return rep, err
	}

	if err := reconcileTemplates(ctx, store, identityDB, defineOwn, &rep); err != nil {
		return rep, err
	}
	if err := reconcileAssignments(ctx, store, identityDB, &rep); err != nil {
		return rep, err
	}

	removed, err := store.SweepStaleAssignments(ctx, generation, reconcileScope())
	if err != nil {
		return rep, err
	}
	rep.StaleRemoved = removed
	return rep, nil
}

// TemplateDefiner writes THIS BUILD's own role definitions into TSM's tables.
//
// Defined here and implemented by the application (bootstrap.seedRoleTemplates)
// for the same reason AuthorityReducer is: the ordering relative to the adopt
// pass is a correctness property of the reconcile, not of the caller, so the seed
// is a step this function runs at the one point it is right rather than a call
// somebody makes near it.
type TemplateDefiner func(ctx context.Context, store *Store) error

// reconcileScope is the tenancy the startup reconcile writes under.
//
// PLATFORM-WIDE, AND SPELLED RATHER THAN IMPLIED. This is not a request: it runs
// at boot, from no principal, and its job is to make this application's tables
// agree with identity's ENTIRE membership table. There is no caller to narrow it
// to, and narrowing it to some organization would leave every other tenant's
// assignments un-restated and then sweep them as stale — a scope mistake here
// empties the mirror rather than leaking from it.
//
// Written as a call to the shared constructor so it appears in
// TestPlatformWideOrgScopeSitesAreReviewed's enumeration and has to be signed
// off there, which is the estate's mechanism for exactly this claim.
func reconcileScope() idstore.OrgScope { return idstore.OrgScopeAllOrganizations() }

// reconcileTemplates makes this application's role definitions complete, and its
// own.
//
// TWO PASSES, IN THIS ORDER, AND THEY MEAN DIFFERENT THINGS.
//
// ADOPT brings across every identity role template this deployment does not
// already hold, preserving identity's uuid so organization_member_roles can store
// the same role_template_id identity's membership row stores. It is an
// insert-if-absent (Store.AdoptTemplate), not the upsert Phase 3a used: identity
// may still supply a definition this deployment has never seen — without one, an
// assignment restated from identity would violate the foreign key and the
// principal would lose their role — but it may no longer redefine one that is
// already here, now that these rows decide authorization.
//
// DEFINE then writes this build's own role -> scope mapping by NAME, over the top
// of whatever was adopted for those names, keeping the adopted uuid. That is the
// app-side seed, and it is why the two apps no longer overwrite each other: the
// name is unique PER APPLICATION here, so TSM's `editor` is TSM's.
//
// There is no delete pass. A template that disappeared from identity is left
// standing rather than dropped, because dropping it would SET NULL the
// assignments referencing it — turning a template somebody removed upstream into
// a silent loss of every role that used it. It is inert (nothing points at it once
// the assignment pass runs) and visible to the operator in the table.
func reconcileTemplates(ctx context.Context, store *Store, identityDB *sql.DB, defineOwn TemplateDefiner, rep *Report) error {
	templates, err := idstore.NewRoleTemplateRepository(identityDB).ListRoleTemplates(ctx)
	if err != nil {
		return fmt.Errorf("approles: reading identity role templates: %w", err)
	}

	for _, rt := range templates {
		if rt == nil {
			continue
		}
		t := templateFromIdentity(rt.ID.String(), rt.Name, rt.DisplayName, rt.Description, rt.Scopes, rt.IsSystem)
		// A name this deployment already has under a DIFFERENT id: identity
		// dropped the template and recreated it. Release the name before the
		// insert, or the unique index rejects it and the reconcile wedges on a
		// row nobody can reach. The definition pass below rewrites the scopes
		// afterwards, so releasing the name does not cost this build its own.
		if err := store.RepointTemplateName(ctx, t.Name, t.ID); err != nil {
			return err
		}
		if err := store.AdoptTemplate(ctx, t); err != nil {
			return err
		}
		rep.TemplatesAdopted++
	}

	if err := defineOwn(ctx, store); err != nil {
		return fmt.Errorf("approles: defining this application's own role templates: %w", err)
	}

	// BOTH COUNTS ARE READ BACK FROM THE TABLE, not inferred from what was meant
	// to be written. len(expectedScopes()) would report the same constant on every
	// boot whether the definer wrote six rows or partially failed, which makes the
	// startup line unable to distinguish the two — and this is the line an operator
	// reads to confirm the phase is in effect.
	held, err := store.ListTemplates(ctx)
	if err != nil {
		return err
	}
	expected := expectedScopes()
	foreign := make([]string, 0)
	defined := 0
	for name := range held {
		if _, ours := expected[name]; ours {
			defined++
			continue
		}
		foreign = append(foreign, name)
	}
	sort.Strings(foreign)
	rep.TemplatesDefined = defined
	rep.ForeignTemplates = foreign
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
			if err := store.SetRole(ctx, orgID, userID, role, reconcileScope()); err != nil {
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

// expectedScopes is this build's own role -> scope mapping, keyed by name. It
// names the roles this application DEFINES; everything else it holds was adopted
// from identity (Report.ForeignTemplates).
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

// ForeignTemplateRemedy is what an operator does about Report.ForeignTemplates.
//
// It is a WARNING and not a startup failure, on purpose. A foreign template is
// not corruption: it is a role name this deployment holds an assignment for and
// does not define — in a shared identity database, one the sibling registry
// seeded — so it means here whatever that application decided it means. Dropping
// it would SET NULL every assignment using it, and failing the boot would take a
// working deployment down to report a state it has been running in for months.
//
// The Phase 3a warning this replaces said the opposite thing, and the difference
// is the phase: then, EVERY role's scopes came from whichever app seeded the
// shared table last, and the remedy was "you may want to own them". Now this
// application defines its own, so the only roles left with somebody else's
// meaning are the ones it never defined at all.
const ForeignTemplateRemedy = "these role names exist in this application's role_templates but are not defined by this build — they were adopted " +
	"from the shared identity schema so that assignments referencing them keep resolving. Their scopes are whatever the application that seeded them " +
	"there decided. Either define them in auth.AppRoleTemplates() so this build owns their meaning, or confirm no principal here should hold them and " +
	"remove the assignments that do."

// DriftQuery is Phase 3a's same-database drift statement, kept for an operator
// with a psql prompt and no binary to hand.
//
// USE CheckDrift INSTEAD wherever a program is doing the asking, including the
// `authz-drift` command that gates this phase. This statement cannot answer two
// questions that matter once reads have moved, which is why it is no longer what
// anything in this repository calls:
//
//   - It names both schemas in ONE statement, so it does not run at all when
//     identity is a separate database (TSM_IDENTITY_DATABASE_*).
//   - It compares role_template_id and nothing else, so two rows agreeing on the
//     id while the two schemas define that id with DIFFERENT SCOPES read as
//     clean. After the flip that is a principal holding the wrong permissions
//     under the right role name.
//
// Three kinds of row come back:
//
//	missing    — identity has the membership, this application records no role
//	stale      — this application records a role identity no longer has
//	mismatched — both have it, with different roles
//
// WHAT AN OPERATOR DOES, NOW THAT THIS IS READ: a `missing` row is a principal
// who has lost access they should have; a `stale` row is one who has kept access
// they should not. Restarting the backend runs Reconcile, which restates every
// assignment and sweeps what identity no longer has — and now says what it
// changed. Drift that SURVIVES a restart means the reconcile is failing rather
// than that a single write slipped, and the startup log will say why.
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
		"templates_adopted", rep.TemplatesAdopted,
		"templates_defined", rep.TemplatesDefined,
		"assignments_restated", rep.AssignmentsRestated,
		"stale_removed", rep.StaleRemoved)
	if len(rep.ForeignTemplates) > 0 {
		slog.Warn("this application holds role templates it does not define",
			"roles", rep.ForeignTemplates, "remedy", ForeignTemplateRemedy)
	}
	// AT WARN, WITH THE PAIRS, AND ONLY WHEN THERE WERE ANY. Every repair listed
	// here is an authorization change this boot made on somebody's behalf: a
	// `missing` row it filled in granted access, a `stale` row it swept withdrew
	// it. A reconcile that repaired nothing is the normal case and says nothing,
	// so a line at all is the signal.
	if rep.PendingRepairs.AssignmentDrift() > 0 {
		slog.Warn("the reconcile repaired role assignments that disagreed with identity",
			"missing", rep.PendingRepairs.Missing,
			"stale", rep.PendingRepairs.Stale,
			"mismatched", rep.PendingRepairs.Mismatched,
			"detail", rep.PendingRepairs.String())
	}
	if rep.PendingRepairs.ScopeDivergent > 0 || len(rep.PendingRepairs.TemplateDrift) > 0 {
		slog.Warn("this application and the shared identity schema define role scopes differently",
			"affected_memberships", rep.PendingRepairs.ScopeDivergent,
			"templates", len(rep.PendingRepairs.TemplateDrift),
			"detail", rep.PendingRepairs.String())
	}
}
