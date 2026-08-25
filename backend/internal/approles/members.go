package approles

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	idstore "github.com/sethbacon/terraform-suite-identity/identity/store"
)

// identityOrgs is the shared library's organization repository, embedded in
// Members under an UNEXPORTED name.
//
// A type alias, not a field, and that is the mechanism rather than a flourish.
// Embedding `*idstore.OrganizationRepository` directly would name the field
// `OrganizationRepository` — exported — and any caller could write
// `members.OrganizationRepository.AddMemberWithParams(...)` to reach the
// unwrapped repository and write identity without writing the mirror. Embedding
// an alias makes the FIELD name the alias's, which is unexported and therefore
// unreachable outside this package, while the repository's exported methods are
// still promoted so every read TSM already performs keeps compiling untouched.
//
// The result is the property this phase needs: with a *Members in hand, the
// mirrored write is the only write you can spell.
type identityOrgs = idstore.OrganizationRepository

// Members is the shared organization repository with TSM's own role mirror
// bolted to every path that can set, change or remove a role.
//
// # Writes here, reads in reads.go
//
// This file is the WRITE set: every method that sets, changes or removes a role,
// each calling the identity leg it replaces with the same arguments, returning
// the same error, and mirroring. It is unchanged by Phase 3b — both places are
// still written under either RoleSource, which is what makes rolling the reads
// back a restart rather than a restore.
//
// The ROLE-CARRYING READS moved in Phase 3b and live in reads.go. Reads with no
// role in them — GetByName, GetByID, List, Count, Search, GetUserOrganizations —
// are still promoted from the embedded repository verbatim, because organizations
// and memberships are facts identity owns and this phase does not touch them.
//
// # The ordering rule
//
// The two legs are on different connections — identity may be another database —
// so they cannot share a transaction and there IS a window between them. The
// rule that decides which leg goes first is: ORDER THE TWO WRITES SO THAT A
// CRASH BETWEEN THEM LEAVES THE LESS PRIVILEGED STATE.
//
//   - A grant (add, or change of role) writes IDENTITY first. A crash leaves the
//     mirror missing an assignment identity has: under-privileged when reads
//     eventually move, and healed by the next Reconcile.
//   - A revocation (remove, remove-all, organization delete) writes the MIRROR
//     first. A crash leaves the mirror missing an assignment identity still has:
//     the same harmless direction. Identity-first would have left a revoked role
//     still recorded here.
//
// In this phase neither window is observable, because nothing reads these
// tables. The rule is applied now because the phase that starts reading them
// does not get to re-choose it.
//
// # A failed mirror leg FAILS THE REQUEST
//
// This inverted in Phase 3b, and it had to. While identity was the authority, a
// failed mirror leg was a row nobody read: reporting it would have told a caller
// their grant failed when it had not, and every retry-happy caller in this estate
// — the SCIM client, the login path, the admin UI — would have replayed a write
// that already applied. So it was logged and swallowed.
//
// Now these tables ARE the authority, and swallowing is a fail-open with a
// success code on it. Concretely: an administrator demotes a principal from
// `admin` to `viewer`, the identity leg commits, the mirror write fails, and the
// old behaviour returned 200 with an audit entry recording the demotion — while
// organization_member_roles still said `admin`, which is now the table every read
// resolves against. The principal keeps administrator scopes, the API said the
// demotion worked, and nothing surfaces it until the drift loop notices or the
// next restart reconciles.
//
// So a mirror failure is returned. Three things make that safe:
//
//   - REVOCATIONS WRITE THE MIRROR FIRST, so a failure there returns BEFORE
//     identity is touched: nothing changed anywhere, and the caller's retry is a
//     retry of an operation that did not happen.
//   - GRANTS WRITE IDENTITY FIRST, so a failure leaves identity ahead of the
//     mirror. The caller sees an error, this application grants nothing new
//     (under-privileged, the safe direction), and Reconcile restates it.
//   - EVERY ONE OF THESE WRITES IS IDEMPOTENT — SetRole is an upsert, the deletes
//     remove nothing when the row is gone — so the retry the error invites cannot
//     double-apply.
//
// The identity leg is NOT rolled back: it cannot be, across a connection
// boundary. The divergence is reported by CheckDrift and repaired by the next
// Reconcile, which now says what it changed.
type Members struct {
	*identityOrgs

	store *Store
	// templates resolves a role template out of identity when the mirror is
	// asked to record an assignment naming one it has never seen. See
	// ensureTemplateByID.
	templates *idstore.RoleTemplateRepository
	// roleSource decides which tables the ROLE-CARRYING READS in reads.go
	// resolve from. Writes are unaffected: both places are written under either
	// value, which is what makes the identity value a working rollback rather
	// than a downgrade to stale data.
	roleSource RoleSource
}

// AuthorityReducer invalidates the credentials that carry a SNAPSHOT of the
// authority a role write has just reduced.
//
// # Why this is a parameter of the mutation and not of the constructor
//
// TSM has two credential families that freeze a principal's authority at issue
// time — JWT sessions (scopes embedded at login) and API keys (scopes frozen on
// the row) — so removing a membership or narrowing a role does not, by itself,
// take anything away (#330). The sweep that does live in internal/credlifecycle,
// which imports THIS package; the reverse import would be a cycle.
//
// So the dependency is inverted, in exactly the shape
// identity/platformadmin.AuditIntentWriter takes: this package defines the
// contract, the application supplies the implementation, and it is passed to the
// MUTATION rather than held on the receiver. That has three consequences worth
// stating:
//
//   - No construction cycle. credlifecycle.Sweeper holds a *Members for its
//     reads; a Members that demanded a Sweeper at construction could never be
//     built.
//   - MANDATORY, enforced by the compiler. These methods shadow the embedded
//     repository's methods BY NAME, so the four-argument call a caller used to
//     write no longer compiles and there is no nil to pass by accident. An
//     optional guard is how a guard goes silently absent.
//   - The FLAVOUR is the caller's. The IdP login paths must sweep keys only —
//     moving the JWT watermark microseconds before minting the session token
//     would revoke the token being issued (see credlifecycle.Sweeper.KeysOnly) —
//     while the admin routes must sweep both, and SCIM deprovisioning sweeps
//     everything unconditionally. A sweep chosen inside this package could not
//     tell them apart; one supplied per call site keeps that documented
//     asymmetry exactly where it was decided.
//
// An error is returned rather than credlifecycle's Outcome because this package
// must not know what a credential family is. It is reported by the method that
// ran the reduction, so a caller that treats an incomplete sweep as fatal — the
// GDPR erasure route does — still can.
// authorityChanged reports whether the write that precedes the sweep ACTUALLY
// changed what the principal may do. It is false only where the caller is
// certain nothing moved -- a removal that removed no row, a reassignment to the
// role already held. Anything uncertain passes true, so a mistake here costs an
// unnecessary sweep rather than a missed reduction.
//
// The distinction exists because the two halves of a sweep are not equally safe
// to run on a no-op. The API-key half re-derives the retained authority and
// keeps every key still covered, so it is harmless. The token half moves a
// PLATFORM-WIDE per-user watermark and ends every session that principal holds,
// everywhere, which on a no-op is pure damage (#491).
type AuthorityReducer func(ctx context.Context, userID string, authorityChanged bool) error

// NewMembers wraps the shared organization repository with TSM's role mirror.
//
// identityDB is where membership and the shared role templates live; appDB is
// where this application's own authorization tables live. They may be the same
// database (the default) or different ones (TSM_IDENTITY_DATABASE_*); nothing
// here assumes either.
//
// A nil appDB yields a Members with NO MIRROR: every write override degrades to
// the identity leg alone and every read override degrades to identity. That is
// for the unit-test rig and for the handful of constructions that predate an app
// connection being available at that point in startup — never for the server,
// where Reconcile's Verify runs first and aborts boot if the tables are absent or
// misrouted.
//
// # Why the source is a MANDATORY parameter
//
// Phase 3b makes "which tables answer a role question" a per-deployment decision
// with a rollback position (RoleSource). Adding it as a third argument rather
// than a default, or a setter, or a package variable, means every construction
// site in the tree had to be edited to state its answer — and a new one cannot
// compile without stating it. That is the same choice AuthorityReducer took, for
// the same reason: an optional guard is how a guard goes silently absent, and the
// failure it would hide here is a whole deployment still authorizing from the
// shared schema while its operators believe it does not.
func NewMembers(identityDB, appDB *sql.DB, source RoleSource) *Members {
	m := &Members{identityOrgs: idstore.NewOrganizationRepository(identityDB), roleSource: source}
	if appDB != nil {
		m.store = NewStore(appDB)
	} else {
		logDegradedSource(source)
	}
	if identityDB != nil {
		m.templates = idstore.NewRoleTemplateRepository(identityDB)
	}
	return m
}

// Store exposes the app-side tables for the reconcile and for tests. It is nil
// when this Members was constructed without an app connection.
func (m *Members) Store() *Store { return m.store }

// AddMemberWithRoleTemplate grants membership with a role template id, and
// mirrors the assignment.
//
// GRANT: identity first (see the ordering rule). The mirror runs only when the
// identity write succeeded — mirroring a grant that did not happen would create
// authority in this app's tables that exists nowhere else.
//
// # Why a GRANT takes an AuthorityReducer
//
// It looks like it cannot need one, and on the identity leg it cannot: that
// statement is a plain INSERT under UNIQUE(organization_id, user_id), so adding
// a principal who is already a member raises a unique violation rather than
// moving their role. Nothing is reduced there, ever.
//
// The MIRROR leg is an upsert (Store.SetRole), and since Phase 3b it writes the
// table that decides authorization. It fires only when the identity insert
// succeeded — so identity had no membership for the pair — but this application
// can still hold a role record for it: that is exactly the `stale` kind
// CheckDrift reports, and it is reachable through a CASCADE in identity, a
// removal by the sibling registry, or a mirror delete that failed. Adding that
// principal back as `viewer` then moves this application's record from whatever
// it stale-held — `admin`, say — down to `viewer`.
//
// That is an authority REDUCTION performed by a method named Add. The API-key
// family happens to be covered in practice (internal/middleware re-derives a
// key owner's live scopes through these same accessors on every request), but a
// session JWT carries a flat scope set frozen at login and is not re-derived —
// so the reduction is real on at least one family, and the parameter is what
// makes it impossible to reach without deciding.
//
// sethbacon/security-orchestration#732 flagged this. The signature is right, and
// the argument that it is "grant-shaped" is an argument from the method's name
// rather than from its statements.
func (m *Members) AddMemberWithRoleTemplate(ctx context.Context, orgID, userID string, roleTemplateID *string, scope idstore.OrgScope, reduce AuthorityReducer) error {
	if err := m.identityOrgs.AddMemberWithRoleTemplate(ctx, orgID, userID, roleTemplateID, scope); err != nil {
		return err
	}
	if err := m.mirrorSetByID(ctx, orgID, userID, roleTemplateID, scope); err != nil {
		return err
	}
	// AFTER the mirror, unlike the reduction methods, which sweep on both
	// outcomes: here the reduction is a side effect of a write that may not have
	// happened, and sweeping before it would revoke credentials for an authority
	// change that then failed.
	return reduceAuthority(ctx, reduce, userID, true)
}

// AddMemberWithParams grants membership by role NAME, and mirrors the assignment.
//
// GRANT: identity first. Takes an AuthorityReducer for the reason
// AddMemberWithRoleTemplate does — it is the same mirror upsert, reached by name.
func (m *Members) AddMemberWithParams(ctx context.Context, orgID, userID, roleTemplateName string, scope idstore.OrgScope, reduce AuthorityReducer) error {
	if err := m.identityOrgs.AddMemberWithParams(ctx, orgID, userID, roleTemplateName, scope); err != nil {
		return err
	}
	if err := m.mirrorSetByName(ctx, orgID, userID, roleTemplateName, scope); err != nil {
		return err
	}
	return reduceAuthority(ctx, reduce, userID, true)
}

// UpdateMemberRoleTemplate changes a member's role template id, and mirrors it.
//
// GRANT: identity first. A reassignment can be a reduction as well as an
// escalation, but the safe ordering for the pair as a whole is the one that
// never leaves this app holding authority identity does not.
func (m *Members) UpdateMemberRoleTemplate(ctx context.Context, orgID, userID string, roleTemplateID *string, scope idstore.OrgScope, reduce AuthorityReducer) error {
	// Read the role currently recorded BEFORE the write, so a reassignment to
	// the role already held can be recognised as the no-op it is (#491).
	//
	// Read through identityOrgs, not through m.GetMember: this compares against
	// the value identityOrgs.UpdateMemberRoleTemplate is about to overwrite, on
	// the same axis. m.GetMember may substitute the app mirror's role id
	// depending on the configured source, and the two spaces are not
	// interchangeable -- comparing across them would silently answer "changed"
	// or "unchanged" on the wrong evidence.
	//
	// A read that FAILS yields true: uncertainty must cost an unnecessary sweep,
	// never a missed reduction.
	authorityChanged := true
	if before, berr := m.identityOrgs.GetMember(ctx, orgID, userID, scope); berr == nil && before != nil {
		authorityChanged = !sameRoleTemplate(before.RoleTemplateID, roleTemplateID)
	}

	err := m.identityOrgs.UpdateMemberRoleTemplate(ctx, orgID, userID, roleTemplateID, scope)
	if err == nil {
		err = m.mirrorSetByID(ctx, orgID, userID, roleTemplateID, scope)
	}
	// A reassignment is a REDUCTION whenever the new template grants less than
	// the old one, and the sweep re-derives the retained set rather than
	// comparing, so it runs on both outcomes for the same reason RemoveMember's
	// does. Only the SESSION half is withheld when nothing moved.
	if serr := reduceAuthority(ctx, reduce, userID, authorityChanged); serr != nil {
		return serr
	}
	return err
}

// sameRoleTemplate compares two nullable role-template ids. Two absent roles are
// the same role; an absent one and a present one are not.
func sameRoleTemplate(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// UpdateMemberRole changes a member's role by NAME, and mirrors it.
//
// GRANT: identity first.
func (m *Members) UpdateMemberRole(ctx context.Context, orgID, userID, roleTemplateName string, scope idstore.OrgScope, reduce AuthorityReducer) error {
	// Same no-op detection as the id variant, on the NAME axis because that is
	// what this method writes (#491). Read through identityOrgs for the same
	// reason: it is identity's own record of the role, which is what
	// identityOrgs.UpdateMemberRole is about to overwrite. Comparing against the
	// app mirror's id would be comparing across two id spaces.
	//
	// This path matters most. Its caller is the IdP group-mapping reconcile on
	// the LOGIN path, so before this a user in a mapped organization had every
	// other session they held ended each time they signed in anywhere.
	authorityChanged := true
	if before, berr := m.identityOrgs.GetMemberWithRole(ctx, orgID, userID, scope); berr == nil && before != nil && before.RoleTemplateName != nil {
		authorityChanged = *before.RoleTemplateName != roleTemplateName
	}

	err := m.identityOrgs.UpdateMemberRole(ctx, orgID, userID, roleTemplateName, scope)
	if err == nil {
		err = m.mirrorSetByName(ctx, orgID, userID, roleTemplateName, scope)
	}
	if serr := reduceAuthority(ctx, reduce, userID, authorityChanged); serr != nil {
		return serr
	}
	return err
}

// RemoveMember withdraws a membership, and removes the mirrored assignment.
//
// REVOCATION: the mirror goes FIRST, so a failure between the two legs leaves
// this app's tables holding less than identity rather than more.
//
// The mirror also runs when the identity leg reports ErrNotFound. That sentinel
// means the membership was already absent there, which is the end state this
// call was asking for — leaving a mirrored assignment behind for it would
// preserve exactly the record the caller asked to be rid of. Every caller in TSM
// already absorbs that sentinel for the same reason.
func (m *Members) RemoveMember(ctx context.Context, orgID, userID string, scope idstore.OrgScope, reduce AuthorityReducer) error {
	// RETURNS BEFORE IDENTITY IS TOUCHED. The mirror is what decides whether this
	// principal may still act here, so a removal that could not clear it has not
	// removed anything — and proceeding would leave identity saying "not a member"
	// while this application still granted the role.
	if err := m.mirrorDelete(ctx, orgID, userID, scope); err != nil {
		return err
	}
	err := m.identityOrgs.RemoveMember(ctx, orgID, userID, scope)
	// The sweep still runs when the removal reported ErrNotFound: it re-derives
	// what the principal RETAINS rather than assuming a row moved, so it is
	// correct on a no-op and reaps a key that was already over-asking.
	//
	// What it must NOT do on a no-op is end that principal's sessions (#491).
	// ErrNotFound means no row was removed, so no authority changed, and the
	// route above absorbs it into a 204 -- which made "DELETE a user who is not
	// a member" a way to sign that user out of every organization, from an
	// organization they were never in.
	if serr := reduceAuthority(ctx, reduce, userID, !errors.Is(err, idstore.ErrNotFound)); serr != nil {
		return serr
	}
	return err
}

// RemoveAllMembershipsForUser strips a user's memberships within scope, and
// removes the mirrored assignments.
//
// REVOCATION, AND MIRRORED TWICE ON PURPOSE. The pre-pass removes the mirror's
// rows for the organizations the scope names before identity is touched, so the
// crash window leaves this app holding less. The post-pass removes exactly the
// organizations identity reports it actually emptied, which is the authoritative
// set and can be narrower than the scope. Both are deletes of the same rows, so
// running both is idempotent; running only the first would over-delete when the
// scope was wider than the effect, and running only the second would put the
// window on the wrong side.
func (m *Members) RemoveAllMembershipsForUser(ctx context.Context, userID string, scope idstore.OrgScope, reduce AuthorityReducer) (idstore.OrgScope, error) {
	if err := m.mirrorDeleteForUser(ctx, userID, scope); err != nil {
		return idstore.OrgScope{}, err
	}
	removed, err := m.identityOrgs.RemoveAllMembershipsForUser(ctx, userID, scope)
	if err != nil {
		return removed, err
	}
	if perr := m.mirrorDeleteForUser(ctx, userID, removed); perr != nil {
		return removed, perr
	}
	return removed, reduceAuthority(ctx, reduce, userID, true)
}

// Delete removes an organization, and removes every mirrored assignment in it.
//
// REVOCATION, AND THE ONE THAT IS NOT A MEMBERSHIP STATEMENT AT ALL.
// identity.organization_members.organization_id is ON DELETE CASCADE, so
// deleting an organization silently withdraws every member's role there. This
// table has no foreign key to cascade with — identity may be another database —
// so the cascade is performed explicitly, and first, per the ordering rule.
//
// Not reached by the guard's method-name list on its own: `Delete` is too
// generic a name to key a class guard on, which is exactly why the guard is
// keyed on the CONSTRUCTOR instead — anything that can reach this repository has
// to come through NewMembers and therefore through this override.
func (m *Members) Delete(ctx context.Context, orgID string, scope idstore.OrgScope, reduce AuthorityReducer) error {
	// SNAPSHOT FIRST. The delete cascades organization_members away, so after it
	// there is nobody left to sweep — and this is the one reduction whose subject
	// is a SET of principals rather than one. Enumerating here rather than at the
	// call site is what stops "remember to list the members before you delete the
	// organization" from being a convention each new caller has to rediscover.
	//
	// A listing failure is fatal: deleting the organization anyway would withdraw
	// every member's authority with no possibility of sweeping the credentials
	// that froze it.
	//
	// m.identityOrgs.ListMembers, NOT the role-overlaying override in reads.go,
	// and deliberately: the only field used below is UserID — WHO is losing
	// authority — which is identity's fact either way. Going through the override
	// would issue a second query to decorate a role nothing here reads.
	members, err := m.identityOrgs.ListMembers(ctx, orgID, scope)
	if err != nil {
		return err
	}
	if err := m.mirrorDeleteForOrganization(ctx, orgID, scope); err != nil {
		return err
	}
	if err := m.identityOrgs.Delete(ctx, orgID, scope); err != nil && !errors.Is(err, idstore.ErrNotFound) {
		return err
	} else if err != nil {
		// Already gone: nothing cascaded, so there is nothing to sweep, and the
		// sentinel is returned for the caller's own idempotence decision.
		return err
	}
	for _, member := range members {
		if serr := reduceAuthority(ctx, reduce, member.UserID, true); serr != nil {
			return serr
		}
	}
	return nil
}

// PurgeUserRoles removes every mirrored assignment for a user, for the one path
// that withdraws authority without going through this repository at all:
// UserRepository.DeleteUser, whose identity.organization_members rows disappear
// by ON DELETE CASCADE.
//
// It is a separate, explicitly-called method rather than another override
// because the write it accompanies belongs to a DIFFERENT repository, and
// wrapping the user repository to catch one method would put a second wrapper on
// every path that only ever reads users. dual_write_class_test.go pins the
// pairing instead: a function that calls DeleteUser and does not call this is
// not certified.
//
// The scope is the CALLER'S, and it is a parameter rather than an assumed
// platform-wide strip. identity's CASCADE reaches every organization, so the
// mirror's counterpart has to be spelled OrgScopeAllOrganizations() at a call
// site that has established the delete applied — which makes it visible to
// TestPlatformWideOrgScopeSitesAreReviewed instead of hidden inside this method.
//
// Idempotent, and safe to call for a user who held nothing.
//
// THE ONE MIRROR WRITE WHOSE FAILURE IS STILL SWALLOWED, and the exception is
// narrow enough to state: the principal has just been deleted from identity, so
// a role record that survives here names a user who can no longer authenticate
// at all. It grants nobody anything, and the reconcile's sweep collects it at the
// next boot. Every OTHER mirror write now fails its request, because every other
// one leaves a live principal holding a role this application would honour.
func (m *Members) PurgeUserRoles(ctx context.Context, userID string, scope idstore.OrgScope) {
	_ = m.mirrorDeleteForUser(ctx, userID, scope)
}

// reduceAuthority runs the caller's credential sweep, refusing a nil one.
//
// FAIL-CLOSED ON A MISSING SWEEP. A nil reducer is not "no sweep needed", it is
// a caller that did not decide, and the whole point of taking this as a
// mandatory parameter is that the answer is never left to a zero value. Every
// method here is one that reduces derived authority; running one with no sweep
// leaves the credentials that froze that authority working.
func reduceAuthority(ctx context.Context, reduce AuthorityReducer, userID string, authorityChanged bool) error {
	if reduce == nil {
		return fmt.Errorf("approles: authority was reduced for user %s with no credential sweep supplied", userID)
	}
	return reduce(ctx, userID, authorityChanged)
}

// mirrorSetByID records an assignment whose role is named by identity's template
// id.
//
// The id is identity's, and TSM's role_templates deliberately carries the SAME
// uuid for every row it copied, so it is usable here as-is. When it is not
// present — a template identity acquired after this deployment's last reconcile,
// which in a shared identity database can be one the SIBLING app created — the
// template is fetched and recorded first, because the foreign key would
// otherwise reject the assignment and the mirror would lose a grant that did
// happen.
func (m *Members) mirrorSetByID(ctx context.Context, orgID, userID string, roleTemplateID *string, scope idstore.OrgScope) error {
	if m.store == nil {
		return nil
	}
	recorded := roleTemplateID
	if roleTemplateID != nil {
		resolved, err := m.resolveTemplateByID(ctx, *roleTemplateID)
		if err != nil {
			slog.Error("approles: could not record the role template for a mirrored assignment",
				"organization_id", orgID, "user_id", userID, "role_template_id", *roleTemplateID, "error", err)
			return err
		}
		recorded = &resolved
	}
	if err := m.store.SetRole(ctx, orgID, userID, recorded, scope); err != nil {
		slog.Error("approles: identity recorded the role assignment but this application's mirror did not",
			"organization_id", orgID, "user_id", userID, "error", err)
		return err
	}
	return nil
}

// mirrorSetByName records an assignment whose role is named by template NAME,
// resolving the name against TSM's OWN role_templates.
//
// Resolved locally rather than by asking identity for the id, because the name
// is what the caller meant and this app's table is what the name must mean HERE.
// In this phase the two resolve to the same row (the reconcile copies identity's
// ids), so the distinction is invisible; from the phase that switches reads
// onward it is the difference between "the editor role" meaning TSM's editor and
// meaning whichever app seeded that name last.
func (m *Members) mirrorSetByName(ctx context.Context, orgID, userID, roleTemplateName string, scope idstore.OrgScope) error {
	if m.store == nil {
		return nil
	}
	id, err := m.store.TemplateIDByName(ctx, roleTemplateName)
	if errors.Is(err, ErrNoTemplate) {
		// The identity write SUCCEEDED with this name, so identity has a
		// template the mirror does not. Copy it across and retry once, rather
		// than dropping the assignment: this is the shared-database case where
		// the sibling app created a role since the last reconcile.
		if id, err = m.adoptTemplateByName(ctx, roleTemplateName); err != nil {
			slog.Error("approles: identity accepted a role name this application's mirror cannot resolve",
				"organization_id", orgID, "user_id", userID, "role", roleTemplateName, "error", err)
			return err
		}
	} else if err != nil {
		slog.Error("approles: could not resolve a role name in this application's mirror",
			"organization_id", orgID, "user_id", userID, "role", roleTemplateName, "error", err)
		return err
	}
	if err := m.store.SetRole(ctx, orgID, userID, &id, scope); err != nil {
		slog.Error("approles: identity recorded the role assignment but this application's mirror did not",
			"organization_id", orgID, "user_id", userID, "role", roleTemplateName, "error", err)
		return err
	}
	return nil
}

// resolveTemplateByID returns the role template id THIS APPLICATION will record
// for an assignment identity expressed with roleTemplateID, adopting the
// definition when this deployment has never seen it.
//
// # It never deletes anything, and that is the point
//
// An earlier version of this called Store.RepointTemplateName first — releasing
// the name from whatever id held it — because Phase 3b's app-side seed can mint a
// LOCAL uuid for a role name identity does not have (Store.DefineTemplate), so
// `operator` can exist here under uuid Y while the sibling later seeds `operator`
// into identity under uuid Z, and the adopt insert would then violate the unique
// index on name.
//
// That fix was worse than the fault. RepointTemplateName DELETEs the row, and
// organization_member_roles.role_template_id is ON DELETE SET NULL — so a request
// GRANTING one principal a role would have silently withdrawn that role from
// EVERY OTHER principal holding it, with no credential sweep and no repair until
// the next boot. The reconcile can afford that statement because its assignment
// pass restates every membership microseconds later; a request path cannot.
// (sethbacon/security-orchestration#732 flagged exactly this, on both helpers.)
//
// The right answer was never to delete: it is to USE THE LOCAL ID. The caller
// named a role, this application defines what that name means here, and under the
// per-app model that definition is the answer — which is the same reasoning
// mirrorSetByName already applies when the caller names the role directly.
func (m *Members) resolveTemplateByID(ctx context.Context, roleTemplateID string) (string, error) {
	present, err := m.store.TemplateExists(ctx, roleTemplateID)
	if err != nil {
		return "", err
	}
	if present {
		return roleTemplateID, nil
	}
	if m.templates == nil {
		return "", ErrNoTemplate
	}
	parsed, err := uuid.Parse(roleTemplateID)
	if err != nil {
		return "", err
	}
	rt, err := m.templates.GetRoleTemplate(ctx, parsed)
	if err != nil {
		return "", err
	}
	if rt == nil {
		return "", ErrNoTemplate
	}
	// The NAME may already be defined here, under a locally-minted id. Use it.
	local, lerr := m.store.TemplateIDByName(ctx, rt.Name)
	if lerr == nil {
		return local, nil
	}
	if !errors.Is(lerr, ErrNoTemplate) {
		return "", lerr
	}
	// Genuinely new here: adopt identity's row, ids included. AdoptTemplate and
	// not UpsertTemplate, for the reason the reconcile's adopt pass uses it —
	// identity may supply a definition this deployment has never seen, and may not
	// redefine one it already holds.
	t := templateFromIdentity(rt.ID.String(), rt.Name, rt.DisplayName, rt.Description, rt.Scopes, rt.IsSystem)
	if err := m.store.AdoptTemplate(ctx, t); err != nil {
		return "", err
	}
	return t.ID, nil
}

// adoptTemplateByName resolves a role NAME the mirror could not resolve, by
// asking identity for it and then resolving identity's id here.
//
// Delegates to resolveTemplateByID rather than inserting directly, so there is
// ONE place that decides which id this application records — and so this path
// cannot grow a delete of its own.
func (m *Members) adoptTemplateByName(ctx context.Context, name string) (string, error) {
	if m.templates == nil {
		return "", ErrNoTemplate
	}
	rt, err := m.templates.GetRoleTemplateByName(ctx, name)
	if err != nil {
		return "", err
	}
	if rt == nil {
		return "", ErrNoTemplate
	}
	return m.resolveTemplateByID(ctx, rt.ID.String())
}

func (m *Members) mirrorDelete(ctx context.Context, orgID, userID string, scope idstore.OrgScope) error {
	if m.store == nil {
		return nil
	}
	if err := m.store.DeleteRole(ctx, orgID, userID, scope); err != nil {
		slog.Error("approles: this application's mirror still records a role that is being withdrawn",
			"organization_id", orgID, "user_id", userID, "error", err)
		return err
	}
	return nil
}

func (m *Members) mirrorDeleteForUser(ctx context.Context, userID string, scope idstore.OrgScope) error {
	if m.store == nil {
		return nil
	}
	if err := m.store.DeleteRolesForUser(ctx, userID, scope); err != nil {
		slog.Error("approles: this application's mirror still records roles that are being withdrawn",
			"user_id", userID, "organizations", scope.OrganizationIDs(), "error", err)
		return err
	}
	return nil
}

func (m *Members) mirrorDeleteForOrganization(ctx context.Context, orgID string, scope idstore.OrgScope) error {
	if m.store == nil {
		return nil
	}
	if err := m.store.DeleteRolesForOrganization(ctx, orgID, scope); err != nil {
		slog.Error("approles: this application's mirror still records roles in an organization being deleted",
			"organization_id", orgID, "error", err)
		return err
	}
	return nil
}

// THE TENANCY CHECK USED TO LIVE HERE, as Members.permits and
// scopeOrganizations, and it no longer does. Both translated the caller's scope
// into an `if` around the mirror leg and left the statements themselves
// unqualified — which closed the hole for the paths that remembered and left the
// data layer unable to refuse the ones that did not. The predicate now lives in
// the statements (Store.andScope), so an out-of-tenancy write matches no row
// instead of being skipped by a caller-side branch, and a new Store accessor
// cannot omit it without failing the class guard.

// templateFromIdentity converts identity's role template into this package's
// transfer shape, preserving the id.
func templateFromIdentity(id, name, displayName string, description *string, scopes []string, isSystem bool) Template {
	return Template{
		ID:          id,
		Name:        name,
		DisplayName: displayName,
		Description: description,
		Scopes:      scopes,
		IsSystem:    isSystem,
	}
}
