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
	// TemplatesDefined is the number of role definitions this build wrote as its
	// own (the app-side seed).
	TemplatesDefined int
	// MembershipsConfirmed is the number of (organization_id, user_id) membership
	// facts read from identity and confirmed in TSM's own
	// organization_member_roles — presence only; the role each row carries is
	// this application's and is not touched. See Store.ConfirmMembership.
	MembershipsConfirmed int
	// StaleRemoved is the number of mirrored assignments deleted because
	// identity no longer has them.
	StaleRemoved int64
	// TemplatesReduced names the role definitions this boot NARROWED, with the
	// scopes before and after and the principals holding them. Empty is the
	// normal case: a build that redefines the same roles reduces nothing. A
	// non-empty one is the single most consequential thing a boot can do to a
	// running deployment's authorization, so it is reported rather than
	// inferred from a diff of two images.
	TemplatesReduced []ReducedTemplate
	// ForeignTemplates names the role templates this application holds but does
	// NOT define — adopted from identity, and therefore meaning here whatever the
	// application that seeded them there decided. See ForeignTemplateRemedy.
	ForeignTemplates []string
	// PendingRepairs is what the two sides disagreed about BEFORE this reconcile
	// changed anything. `missing` rows are about to be confirmed as members with
	// no role and `stale` rows are about to be swept; `mismatched` rows are
	// REPORTED and left standing, because the role this application records is
	// the authority now — see Reconcile.
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
// From auth.AppRoleTemplates(), written by defineOwn, AND FROM NOWHERE ELSE.
// The Phase 3b reconcile still ADOPTED identity's role templates first — ids and
// scopes — so that assignments restated from identity resolved here, which kept
// identity.role_templates a read this application depended on at every boot.
// That read is retired: the reconcile no longer lists identity's templates at
// all, a role this build does not define no longer enters this table, and what a
// role name grants HERE is decided by this build alone on every topology.
//
// Roles adopted by EARLIER builds are left standing, and named: ForeignTemplates
// lists every role this application holds but does not define. See
// ForeignTemplateRemedy.
//
// # Identity's role opinion is no longer restated either
//
// Phase 3b's assignment pass copied identity.organization_members.role_template_id
// over this application's rows every boot. That treated identity's record of the
// ROLE as repair data, which was right while the two id spaces were kept aligned
// by the adopt pass and wrong the moment they are not — and, more fundamentally,
// identity owns the membership FACT, not the role. So the scan now reads only
// (organization_id, user_id) and confirms presence (Store.ConfirmMembership):
// a membership this application has never seen arrives as a member with NO role
// (fail-closed, loud, grantable by an administrator), one it already records
// keeps this application's role, and one identity no longer has is swept. A
// role divergence between the two sides is REPORTED by CheckDrift rather than
// repaired — this application's record is the authority now.
//
// # Ordering
//
// Verify, then the pending-repair report, then DEFINE this build's own
// templates, then confirm memberships, then the sweep.
//
// Templates before memberships because organization_member_roles.role_template_id
// has a real foreign key to them. The sweep is last because it deletes everything
// the membership pass did not confirm, and a pass that did not COMPLETE must not
// be allowed to conclude that the rest is stale. Any error before the sweep
// returns without sweeping.
//
// # What changed now that reads depend on this
//
// In Phase 3a this function rebuilt a table nothing read. Two of the things it
// did were free then and are not free now:
//
//   - IT OVERWROTE ROLE DEFINITIONS from identity, every boot. That was correct
//     while identity's definitions WERE the effective ones. Now they are not, and
//     identity no longer supplies definitions here at all — the adopt pass is
//     retired along with every other read of identity.role_templates.
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
func Reconcile(ctx context.Context, appDB, identityDB *sql.DB, defineOwn TemplateDefiner, reduce TemplateAuthorityReducer) (Report, error) {
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
	if reduce == nil {
		return rep, ErrNoTemplateAuthorityReducer
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

	if err := reconcileTemplates(ctx, store, defineOwn, reduce, &rep); err != nil {
		return rep, err
	}
	if err := reconcileMemberships(ctx, store, identityDB, &rep); err != nil {
		return rep, err
	}

	removed, err := store.SweepStaleAssignments(ctx, generation, reconcileScope())
	if err != nil {
		return rep, err
	}
	rep.StaleRemoved = removed
	return rep, nil
}

// TemplateDefiner SUPPLIES this build's own role definitions. It does not write
// them.
//
// Defined here and implemented by the application (bootstrap.appRoleDefinitions)
// for the same reason AuthorityReducer is: the ordering relative to everything
// else the reconcile does is a correctness property of the reconcile, not of the
// caller, so the seed is a step this function runs at the one point it is right
// rather than a call somebody makes near it.
//
// IT RETURNS DEFINITIONS RATHER THAN WRITING THEM, since #557. While it held a
// *Store and wrote through it, the definitions this function judged for an
// authority reduction and the definitions that reached the table were two
// different things connected only by the definer's good behaviour: a definer
// that wrote one extra template, or wrote different scopes than it returned,
// would have its narrowing land with nothing invalidated. Returning the slice
// makes them the same values by construction. It also removes the reason
// Store.defineTemplate had to be exported, which is what closed the bypass.
type TemplateDefiner func(ctx context.Context) ([]Template, error)

// ReducedTemplate is one role definition whose authority this boot is about to
// REDUCE, together with the principals that reduction lands on.
//
// Was and Now are the scope lists before and after. Holders are the users this
// application has assigned the template, read from its own
// organization_member_roles under the reconcile's platform-wide scope — every
// tenant, because a role template has no organization of its own and narrowing
// one narrows it everywhere at once.
type ReducedTemplate struct {
	ID      string
	Name    string
	Was     []string
	Now     []string
	Holders []string
}

// ErrNoTemplateAuthorityReducer is the refusal of a reconcile that was given no
// TemplateAuthorityReducer.
//
// A SENTINEL rather than a bare error string, for the reason
// ErrNoCredentialDecision is one in the shared module: a test that asserts only
// "some error came back" passes just as well when the reconcile ran and failed
// for an unrelated reason — a mocked statement it did not expect, say — which is
// exactly how a fail-closed guard gets verified into existence without ever
// being verified. This one was: the first version of its test passed against a
// build that accepted a nil reducer.
var ErrNoTemplateAuthorityReducer = errors.New("approles: no template-authority reducer supplied: a build that narrows " +
	"a role's scopes would reduce every holder's authority while their existing sessions keep the scopes it removed, " +
	"and nothing would say so. Pass NoTemplateAuthorityReduction to state that this caller has no credentials to invalidate")

// TemplateAuthorityReducer invalidates the credentials derived from role
// templates whose authority this boot is about to reduce.
//
// The inversion, and the reasons, are AuthorityReducer's in members.go: this
// package must not know what a credential family is, `credlifecycle` imports
// `approles` so the dependency cannot run the other way, and the flavour is the
// caller's to choose. What is different here is WHEN it runs — BEFORE the
// definitions are written, not after.
//
// THE ORDER IS THE CONTRACT. A narrowing that lands before its credentials are
// invalidated is precisely the defect (#557): holders keep exercising the
// removed scopes from tokens minted under the old definition. Running the
// reducer first means the failure mode of a crash in between is the harmless
// one — sessions ended for a narrowing that did not land, which the next boot
// recomputes identically because the pre-image is still in the table. It also
// means an error here STOPS the write: a build whose credentials could not be
// invalidated does not get to narrow the role.
//
// A reducer is called only when something was genuinely reduced. Widening a
// role, reordering its scope list, or re-seeding the identical definition calls
// nothing, because AuthorityRetained answers on scope semantics rather than
// slice identity — and ending every session in the estate for a boot that
// granted MORE would be pure damage.
type TemplateAuthorityReducer func(ctx context.Context, reduced []ReducedTemplate) error

// NoTemplateAuthorityReduction is the DELIBERATE opt-out: this caller has no
// credentials derived from role templates to invalidate.
//
// It exists because nil must not mean "nothing to do". A nil reducer is a caller
// that did not decide, and an optional guard is how a guard goes silently
// absent — the same reasoning NoAppCredentials applies in the shared identity
// module and approles already applies to a nil AuthorityReducer. Passing this
// function by name puts the decision in the diff.
func NoTemplateAuthorityReduction(context.Context, []ReducedTemplate) error { return nil }

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
// ONE PASS: DEFINE writes this build's own role -> scope mapping by NAME
// (Store.DefineTemplate), replacing the definition and preserving whatever uuid
// this deployment already carries for the name — so existing assignments keep
// resolving — and minting a fresh uuid for a name it has never held.
//
// The ADOPT pass that used to precede it is retired: it was the boot-time read
// of identity.role_templates, and per-app authorization has no use for the
// sibling's definitions. A template adopted by an earlier build is left standing
// rather than dropped, because dropping it would SET NULL the assignments
// referencing it — turning old adoptions into a silent loss of every role that
// used them. It is inert once no assignment points at it, it is visible to the
// operator in the table, and it is reported below (ForeignTemplates).
func reconcileTemplates(ctx context.Context, store *Store, defineOwn TemplateDefiner, reduce TemplateAuthorityReducer, rep *Report) error {
	definitions, err := defineOwn(ctx)
	if err != nil {
		return fmt.Errorf("approles: obtaining this application's own role definitions: %w", err)
	}

	// THE PRE-IMAGE, READ BEFORE ANYTHING IS WRITTEN. The blind upsert below
	// overwrites `scopes` unconditionally, so after it runs there is no record
	// anywhere of what the role used to grant: not in the row, not in
	// updated_at (re-stamped every boot whether or not anything changed), and
	// not in CheckDrift, which compares assignments and deliberately not
	// definitions. If this read moves below the write, the reduction becomes
	// undetectable rather than merely undetected.
	prior, err := store.ListTemplates(ctx)
	if err != nil {
		return err
	}

	reduced, err := reducedTemplates(ctx, store, prior, definitions)
	if err != nil {
		return err
	}
	if len(reduced) > 0 {
		// BEFORE THE WRITE. See TemplateAuthorityReducer: an error here stops
		// the narrowing, so a build whose credentials could not be invalidated
		// does not get to reduce the role.
		if err := reduce(ctx, reduced); err != nil {
			return fmt.Errorf("approles: invalidating credentials for role definitions this build narrows: %w", err)
		}
		for _, r := range reduced {
			rep.TemplatesReduced = append(rep.TemplatesReduced, r)
		}
	}

	for _, def := range definitions {
		if err := store.defineTemplate(ctx, def); err != nil {
			return fmt.Errorf("approles: defining this application's own role templates: %w", err)
		}
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
	// OURS IS THE SLICE THE DEFINER SUPPLIED, not expectedScopes(). They agree
	// today — bootstrap.appRoleDefinitions is built from auth.AppRoleTemplates()
	// and so was expectedScopes() — but agreeing is not the same as being one
	// fact. A definer that supplied a different set would have its templates
	// written and then counted as FOREIGN, which reads as "this application
	// holds roles it does not define" about the roles it just defined.
	expected := make(map[string]struct{}, len(definitions))
	for _, def := range definitions {
		expected[def.Name] = struct{}{}
	}
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

// reconcileMemberships confirms every identity membership FACT in TSM's own
// organization_member_roles — presence only. See Store.ConfirmMembership for
// what confirming does and deliberately does not do to the role each row
// carries.
//
// The read is raw SQL against the identity connection because the shared store
// has no "every membership" accessor — GetUserMemberships is per principal, and
// calling it once per user would be a query per user to answer a question that
// is one scan. It is a READ of a table this application already reads through
// that store on every login; nothing here writes identity, and — deliberately —
// the SELECT no longer carries role_template_id: the membership fact is the only
// thing identity still supplies to these tables.
func reconcileMemberships(ctx context.Context, store *Store, identityDB *sql.DB, rep *Report) error {
	const q = `
		SELECT organization_id, user_id
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
			if err := rows.Scan(&orgID, &userID); err != nil {
				_ = rows.Close()
				return fmt.Errorf("approles: reading an identity membership: %w", err)
			}
			if err := store.ConfirmMembership(ctx, orgID, userID, reconcileScope()); err != nil {
				_ = rows.Close()
				return err
			}
			lastOrg, lastUser = orgID, userID
			page++
			rep.MembershipsConfirmed++
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			// Returned, NOT swallowed. The caller must not reach the sweep on a
			// partial pass: everything this scan did not confirm would look
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

// reducedTemplates reports which of this build's definitions REDUCE the
// authority the deployment currently records for that name, and who holds them.
//
// The comparison is idstore.AuthorityRetained, the shared module's predicate,
// asked in the direction that matters: is everything the role USED to grant
// still granted by what it is about to grant? A widening answers yes, so does a
// reorder, and so does an identical re-seed — none of them reduce anybody's
// authority, and invalidating credentials for them would end every holder's
// session for a change that took nothing away.
//
// auth.ReadWritePairs() is passed rather than nil. With no pairs the predicate
// loses write-implies-read, so promoting a role from "state:read" to
// "state:write" would read as a REDUCTION and log every holder out of a change
// that granted them more.
//
// A name the deployment does not hold yet is skipped: there is no prior
// authority to reduce, and on a first boot every name is in that position.
func reducedTemplates(ctx context.Context, store *Store, prior map[string]Template, definitions []Template) ([]ReducedTemplate, error) {
	pairs := auth.ReadWritePairs()
	var reduced []ReducedTemplate
	for _, def := range definitions {
		was, held := prior[def.Name]
		if !held {
			continue
		}
		if idstore.AuthorityRetained(was.Scopes, def.Scopes, pairs) {
			continue
		}
		// The id comes from the PRIOR row, not the definition: defineTemplate
		// conflicts on name and deliberately preserves the existing uuid, so
		// the row assignments point at is the one already there. Reading the
		// definition's id would find the empty string the seed supplies.
		holders, err := store.HoldersOfTemplate(ctx, was.ID, reconcileScope())
		if err != nil {
			return nil, err
		}
		reduced = append(reduced, ReducedTemplate{
			ID: was.ID, Name: def.Name, Was: was.Scopes, Now: def.Scopes, Holders: holders,
		})
	}
	return reduced, nil
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
// application defines its own, NO new foreign template can arrive (the adopt
// pass and the mirror's adopt-on-first-use are both retired), so the only roles
// left with somebody else's meaning are ones an EARLIER build adopted.
const ForeignTemplateRemedy = "these role names exist in this application's role_templates but are not defined by this build — an earlier build adopted " +
	"them from the shared identity schema so that assignments referencing them kept resolving. Their scopes are whatever the application that seeded them " +
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
//
// Three kinds of row come back:
//
//	missing    — identity has the membership, this application records no role
//	stale      — this application records a role identity no longer has
//	mismatched — both have it, recording different role template ids
//
// WHAT AN OPERATOR DOES, NOW THAT THIS IS READ: a `missing` row is a principal
// who has lost access they should have; a `stale` row is one who has kept access
// they should not. Restarting the backend runs Reconcile, which confirms every
// membership identity still has and sweeps the ones it no longer does — and says
// what it changed. A `mismatched` row is NOT repaired by a restart: this
// application's role record is the authority, and identity's role column is
// vestigial until Phase 4 removes it — see DriftMismatched for the one topology
// where mismatches are standing rather than a lost write.
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
		"templates_defined", rep.TemplatesDefined,
		"memberships_confirmed", rep.MembershipsConfirmed,
		"stale_removed", rep.StaleRemoved)
	// AT WARN, ALWAYS, AND NAMING THE SCOPES. A boot that narrows a role changes
	// what every holder may do, and the operator's only other evidence is a diff
	// between two images. The count of ended sessions is included because it is
	// the blast radius: "editor lost state:write, 412 principals logged out" is
	// actionable in a way that "templates_defined=6" is not.
	for _, r := range rep.TemplatesReduced {
		slog.Warn("this build narrows a role template; holders' sessions were ended before the change landed",
			"role", r.Name, "was", r.Was, "now", r.Now, "holders", len(r.Holders))
	}
	if len(rep.ForeignTemplates) > 0 {
		slog.Warn("this application holds role templates it does not define",
			"roles", rep.ForeignTemplates, "remedy", ForeignTemplateRemedy)
	}
	// AT WARN, WITH THE PAIRS, AND ONLY WHEN THERE WERE ANY. Every record listed
	// here is a disagreement this boot acted on or chose to leave standing: a
	// `missing` row was confirmed as a member with NO role (an administrator
	// grants from there), a `stale` row was swept, and a `mismatched` row KEEPS
	// this application's role — reported so the divergence with identity's
	// vestigial role column is visible, never repaired from it. A reconcile with
	// nothing to say is the normal case and says nothing, so a line at all is
	// the signal.
	if rep.PendingRepairs.AssignmentDrift() > 0 {
		slog.Warn("this application's role records and identity's membership table disagreed at boot",
			"missing", rep.PendingRepairs.Missing,
			"stale", rep.PendingRepairs.Stale,
			"mismatched", rep.PendingRepairs.Mismatched,
			"detail", rep.PendingRepairs.String())
	}
}
