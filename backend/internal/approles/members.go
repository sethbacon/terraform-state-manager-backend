package approles

import (
	"context"
	"database/sql"
	"errors"
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
// # Every read is unchanged
//
// Reads are promoted from the embedded repository verbatim — GetByName,
// CheckMembership, ListMembers, GetUserMemberships, GetUserCombinedScopes and
// the rest. Phase 3a does not move a single read. What is overridden below is
// exactly the write set, and each override calls the identity leg it replaces
// with the same arguments and returns the same error.
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
// # A failed mirror leg does not fail the request
//
// The identity leg has already committed and cannot be rolled back across the
// connection boundary. Returning an error would tell the caller their grant
// failed when it did not, and every retry-happy caller in this estate — the SCIM
// client, the login path, the admin UI — would replay a write that already
// applied. So the mirror's failure is logged at ERROR, with the pair it could
// not record, and the operation reports what actually happened to identity. The
// divergence is transient by construction: Reconcile restates every assignment
// at the next startup, and driftQuery (reconcile.go) is what an operator runs
// to see one before then.
type Members struct {
	*identityOrgs

	store *Store
	// templates resolves a role template out of identity when the mirror is
	// asked to record an assignment naming one it has never seen. See
	// ensureTemplateByID.
	templates *idstore.RoleTemplateRepository
}

// NewMembers wraps the shared organization repository with TSM's role mirror.
//
// identityDB is where membership and the shared role templates live; appDB is
// where this application's own authorization tables live. They may be the same
// database (the default) or different ones (TSM_IDENTITY_DATABASE_*); nothing
// here assumes either.
//
// A nil appDB yields a Members with NO MIRROR: every override degrades to the
// identity leg alone. That is for the unit-test rig and for the handful of
// constructions that predate an app connection being available at that point in
// startup — never for the server, where Reconcile's Verify runs first and aborts
// boot if the tables are absent or misrouted.
func NewMembers(identityDB, appDB *sql.DB) *Members {
	m := &Members{identityOrgs: idstore.NewOrganizationRepository(identityDB)}
	if appDB != nil {
		m.store = NewStore(appDB)
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
func (m *Members) AddMemberWithRoleTemplate(ctx context.Context, orgID, userID string, roleTemplateID *string, scope idstore.OrgScope) error {
	if err := m.identityOrgs.AddMemberWithRoleTemplate(ctx, orgID, userID, roleTemplateID, scope); err != nil {
		return err
	}
	m.mirrorSetByID(ctx, orgID, userID, roleTemplateID)
	return nil
}

// AddMemberWithParams grants membership by role NAME, and mirrors the assignment.
//
// GRANT: identity first.
func (m *Members) AddMemberWithParams(ctx context.Context, orgID, userID, roleTemplateName string, scope idstore.OrgScope) error {
	if err := m.identityOrgs.AddMemberWithParams(ctx, orgID, userID, roleTemplateName, scope); err != nil {
		return err
	}
	m.mirrorSetByName(ctx, orgID, userID, roleTemplateName)
	return nil
}

// UpdateMemberRoleTemplate changes a member's role template id, and mirrors it.
//
// GRANT: identity first. A reassignment can be a reduction as well as an
// escalation, but the safe ordering for the pair as a whole is the one that
// never leaves this app holding authority identity does not.
func (m *Members) UpdateMemberRoleTemplate(ctx context.Context, orgID, userID string, roleTemplateID *string, scope idstore.OrgScope) error {
	if err := m.identityOrgs.UpdateMemberRoleTemplate(ctx, orgID, userID, roleTemplateID, scope); err != nil {
		return err
	}
	m.mirrorSetByID(ctx, orgID, userID, roleTemplateID)
	return nil
}

// UpdateMemberRole changes a member's role by NAME, and mirrors it.
//
// GRANT: identity first.
func (m *Members) UpdateMemberRole(ctx context.Context, orgID, userID, roleTemplateName string, scope idstore.OrgScope) error {
	if err := m.identityOrgs.UpdateMemberRole(ctx, orgID, userID, roleTemplateName, scope); err != nil {
		return err
	}
	m.mirrorSetByName(ctx, orgID, userID, roleTemplateName)
	return nil
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
func (m *Members) RemoveMember(ctx context.Context, orgID, userID string, scope idstore.OrgScope) error {
	m.mirrorDelete(ctx, orgID, userID)
	return m.identityOrgs.RemoveMember(ctx, orgID, userID, scope)
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
func (m *Members) RemoveAllMembershipsForUser(ctx context.Context, userID string, scope idstore.OrgScope) (idstore.OrgScope, error) {
	m.mirrorDeleteForUser(ctx, userID, scopeOrganizations(scope))
	removed, err := m.identityOrgs.RemoveAllMembershipsForUser(ctx, userID, scope)
	if err != nil {
		return removed, err
	}
	m.mirrorDeleteForUser(ctx, userID, scopeOrganizations(removed))
	return removed, nil
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
func (m *Members) Delete(ctx context.Context, orgID string, scope idstore.OrgScope) error {
	m.mirrorDeleteForOrganization(ctx, orgID)
	return m.identityOrgs.Delete(ctx, orgID, scope)
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
// Idempotent, and safe to call for a user who held nothing.
func (m *Members) PurgeUserRoles(ctx context.Context, userID string) {
	m.mirrorDeleteForUser(ctx, userID, nil)
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
func (m *Members) mirrorSetByID(ctx context.Context, orgID, userID string, roleTemplateID *string) {
	if m.store == nil {
		return
	}
	if roleTemplateID != nil {
		if err := m.ensureTemplateByID(ctx, *roleTemplateID); err != nil {
			slog.Error("approles: could not record the role template for a mirrored assignment",
				"organization_id", orgID, "user_id", userID, "role_template_id", *roleTemplateID, "error", err)
			return
		}
	}
	if err := m.store.SetRole(ctx, orgID, userID, roleTemplateID); err != nil {
		slog.Error("approles: identity recorded the role assignment but this application's mirror did not",
			"organization_id", orgID, "user_id", userID, "error", err)
	}
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
func (m *Members) mirrorSetByName(ctx context.Context, orgID, userID, roleTemplateName string) {
	if m.store == nil {
		return
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
			return
		}
	} else if err != nil {
		slog.Error("approles: could not resolve a role name in this application's mirror",
			"organization_id", orgID, "user_id", userID, "role", roleTemplateName, "error", err)
		return
	}
	if err := m.store.SetRole(ctx, orgID, userID, &id); err != nil {
		slog.Error("approles: identity recorded the role assignment but this application's mirror did not",
			"organization_id", orgID, "user_id", userID, "role", roleTemplateName, "error", err)
	}
}

// ensureTemplateByID copies identity's role template into TSM's own table when
// the id is not already there.
func (m *Members) ensureTemplateByID(ctx context.Context, roleTemplateID string) error {
	present, err := m.store.TemplateExists(ctx, roleTemplateID)
	if err != nil {
		return err
	}
	if present {
		return nil
	}
	if m.templates == nil {
		return ErrNoTemplate
	}
	parsed, err := uuid.Parse(roleTemplateID)
	if err != nil {
		return err
	}
	rt, err := m.templates.GetRoleTemplate(ctx, parsed)
	if err != nil {
		return err
	}
	if rt == nil {
		return ErrNoTemplate
	}
	return m.store.UpsertTemplate(ctx, templateFromIdentity(rt.ID.String(), rt.Name, rt.DisplayName, rt.Description, rt.Scopes, rt.IsSystem))
}

// adoptTemplateByName copies identity's role template into TSM's own table and
// returns its id, for a name the mirror could not resolve.
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
	t := templateFromIdentity(rt.ID.String(), rt.Name, rt.DisplayName, rt.Description, rt.Scopes, rt.IsSystem)
	if err := m.store.UpsertTemplate(ctx, t); err != nil {
		return "", err
	}
	return t.ID, nil
}

func (m *Members) mirrorDelete(ctx context.Context, orgID, userID string) {
	if m.store == nil {
		return
	}
	if err := m.store.DeleteRole(ctx, orgID, userID); err != nil {
		slog.Error("approles: this application's mirror still records a role that is being withdrawn",
			"organization_id", orgID, "user_id", userID, "error", err)
	}
}

func (m *Members) mirrorDeleteForUser(ctx context.Context, userID string, orgIDs []string) {
	if m.store == nil {
		return
	}
	if err := m.store.DeleteRolesForUser(ctx, userID, orgIDs); err != nil {
		slog.Error("approles: this application's mirror still records roles that are being withdrawn",
			"user_id", userID, "organizations", orgIDs, "error", err)
	}
}

func (m *Members) mirrorDeleteForOrganization(ctx context.Context, orgID string) {
	if m.store == nil {
		return
	}
	if err := m.store.DeleteRolesForOrganization(ctx, orgID); err != nil {
		slog.Error("approles: this application's mirror still records roles in an organization being deleted",
			"organization_id", orgID, "error", err)
	}
}

// scopeOrganizations translates an OrgScope into the argument
// Store.DeleteRolesForUser takes: nil for "every organization", the allowlist
// otherwise.
//
// The nil/slice distinction is the same one OrgScope makes and must not be
// flattened: OrgScopeAllOrganizations reaches everywhere, and a scope that
// matches nothing must remove nothing rather than everything. An empty non-nil
// slice is returned for the latter, which DeleteRolesForUser treats as a no-op.
func scopeOrganizations(scope idstore.OrgScope) []string {
	if scope.IsAllOrganizations() {
		return nil
	}
	ids := scope.OrganizationIDs()
	if ids == nil {
		return []string{}
	}
	return ids
}

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
