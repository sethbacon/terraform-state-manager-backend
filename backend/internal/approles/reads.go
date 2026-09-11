package approles

import (
	"context"
	"errors"
	"log/slog"

	idauth "github.com/sethbacon/terraform-suite-identity/identity/auth"
	idmodels "github.com/sethbacon/terraform-suite-identity/identity/models"
	idstore "github.com/sethbacon/terraform-suite-identity/identity/store"
)

// Phase 3b: THE READS MOVE.
//
// Phase 3a made TSM's own tables a mirror and left every authorization decision
// reading identity.organization_members joined to identity.role_templates. This
// file is the switch. From here, "which role does this principal hold in this
// application" is answered by organization_member_roles joined to TSM's own
// role_templates, and identity answers only "is this principal a member of this
// organization" — the fact it owns.
//
// # Why an overlay and not a rewritten query
//
// Membership is still identity's. So each override calls the SHARED repository
// method it replaces — unchanged, with the caller's OrgScope — and then replaces
// the role columns of the rows that came back. Every tenancy predicate, ORDER BY,
// ErrNotFound sentinel and empty-slice convention the library established is
// therefore still the library's, and the diff is confined to the four fields that
// carry a role.
//
// Rewriting the queries against TSM's tables was the other option and is
// rejected: the shared repository's scope handling is where #138/#161/#162 were
// found and fixed, and a second hand-rolled copy of it here would be a second
// place for them to come back. The overlay cannot widen a read — it only
// decorates rows the scoped identity read already returned.
//
// # The direction a gap fails in
//
// A membership identity has, with NO row in organization_member_roles, resolves
// to NO role and therefore NO scopes. That is deliberate and it is the safe
// direction: a gap in the mirror costs a principal access they should have, and
// is loud (they cannot do their job), rather than granting access they should not
// have, which is silent. It is also exactly what CheckDrift reports as `missing`,
// and what the gate in cmd/server (authz-drift) reports on.
//
// # There is no identity position any more
//
// Phase 3b shipped with a rollback lever, TSM_AUTHZ_ROLE_SOURCE=identity, that put
// every role read back on the shared schema. It was retired in #599 (the head of
// sethbacon/terraform-suite-identity#206 Phase 4): it was this application's last read of
// the shared role_templates, and while it existed the sibling registry could not
// stop seeding that table — because a lever that reads a table nobody keeps
// current is not a rollback, it is a silent downgrade to stale data.
//
// So a Members has exactly one way to answer a role question: this application's
// tables. A Members constructed WITHOUT an application connection cannot answer
// it at all, and it refuses rather than guesses — every role-carrying read on it
// returns ErrNoAppStore. The alternative, falling back to identity's role columns,
// is precisely the path this file exists to have closed, and restoring it for a
// nil store would reopen it on exactly the construction sites nobody is looking
// at.
//
// # Go has no virtual dispatch, and that is the trap this file exists to avoid
//
// GetUserCombinedScopes, GetUserScopesForOrg, OrgScopeForUser and CheckMembership
// are DERIVED: the shared library implements each by calling another method on
// its own receiver. Overriding only the base reads would leave those four
// promoted from the embedded repository, still calling the embedded repository's
// base reads, and still answering from identity — while every test of the base
// reads passed. The result would be a principal whose /auth/me shows one role and
// whose session token carries another, with nothing in the request path saying
// so.
//
// So every derived method is re-implemented here over m, and
// TestEveryRoleCarryingReadIsOverridden (dual_write_class_test.go, axis 5) refuses
// a tree in which any role-carrying method of the shared repository is left
// promoted — deriving that list from the LIBRARY'S OWN SOURCE, so an upgrade that
// adds a new one fails the guard instead of silently reading identity.

// Role is one resolved role, as TSM's own tables hold it.
//
// TemplateID is nil for a member recorded with no role — a state identity can
// represent too (organization_members.role_template_id is nullable), so the
// overlay must be able to reproduce it rather than turning it into "no row".
type Role struct {
	TemplateID  *string
	Name        *string
	DisplayName *string
	Scopes      []string
}

// GetUserMemberships returns a user's memberships with the role each carries in
// THIS application.
//
// UNSCOPED, as the shared library's is, and for its reason: this is authority
// derivation and a scope parameter would ask the caller for the answer. The
// overlay reads the whole of one user's row set from the app tables, so the
// platform-wide scope is spelled — see roleReadScope.
func (m *Members) GetUserMemberships(ctx context.Context, userID string) ([]*idmodels.UserMembership, error) {
	if m.store == nil {
		return nil, ErrNoAppStore
	}
	rows, err := m.identityOrgs.GetUserMemberships(ctx, userID)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		// Nothing to decorate; the library's empty-slice convention passes
		// through untouched.
		return rows, nil
	}
	roles, err := m.store.RolesForUser(ctx, userID, roleReadScope())
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		if row == nil {
			continue
		}
		applyToUserMembership(row, roles[row.OrganizationID])
	}
	return rows, nil
}

// GetUserCombinedScopes returns the flat, cross-organization union of the scopes
// this application grants a principal.
//
// RE-IMPLEMENTED, NOT PROMOTED. The library computes this from its OWN
// GetUserMemberships; promoted, it would have gone on reading identity while the
// override above read the app tables, and the two would have disagreed on the
// hottest authorization path in the process — the API-key scope cap in
// internal/middleware runs it on every request. The body is the library's,
// verbatim, over m.
func (m *Members) GetUserCombinedScopes(ctx context.Context, userID string) (idauth.GlobalScopes, error) {
	memberships, err := m.GetUserMemberships(ctx, userID)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool)
	for _, mem := range memberships {
		for _, s := range mem.RoleTemplateScopes {
			seen[s] = true
		}
	}
	scopes := make(idauth.GlobalScopes, 0, len(seen))
	for s := range seen {
		scopes = append(scopes, s)
	}
	return scopes, nil
}

// OrgScopeForUser resolves the organizations in which a principal holds
// `required` IN THIS APPLICATION.
//
// RE-IMPLEMENTED for the same reason as GetUserCombinedScopes: the library
// derives it from its own GetUserMemberships. This is the resolver every scoped
// admin route is built on, so leaving it promoted would have kept TSM's tenancy
// decisions on identity's roles while its scope decisions moved.
func (m *Members) OrgScopeForUser(ctx context.Context, userID, required string, rwPairs idauth.ReadWritePairs) (idstore.OrgScope, error) {
	if userID == "" {
		return idstore.OrgScope{}, nil
	}
	memberships, err := m.GetUserMemberships(ctx, userID)
	if err != nil {
		return idstore.OrgScope{}, err
	}
	orgIDs := make([]string, 0, len(memberships))
	for _, mem := range memberships {
		if idauth.HasScope(mem.RoleTemplateScopes, required, rwPairs) {
			orgIDs = append(orgIDs, mem.OrganizationID)
		}
	}
	return idstore.OrgScopeOrganizations(orgIDs...), nil
}

// GetMemberWithRole returns one membership with the role it carries here.
func (m *Members) GetMemberWithRole(ctx context.Context, orgID, userID string, scope idstore.OrgScope) (*idmodels.OrganizationMemberWithUser, error) {
	if m.store == nil {
		return nil, ErrNoAppStore
	}
	row, err := m.identityOrgs.GetMemberWithRole(ctx, orgID, userID, scope)
	if err != nil {
		return nil, err
	}
	role, _, err := m.store.RoleForPair(ctx, orgID, userID, scope)
	if err != nil {
		return nil, err
	}
	applyToMemberWithUser(row, role)
	return row, nil
}

// GetUserScopesForOrg returns the scopes this application grants a principal
// inside ONE organization.
//
// RE-IMPLEMENTED: the library derives it from its own GetMemberWithRole. This is
// what requireOrgScope re-derives per request (internal/api/admin_org_scope.go),
// so a promoted copy would have left every per-organization admin decision on
// identity's roles.
func (m *Members) GetUserScopesForOrg(ctx context.Context, userID, orgID string) (idauth.OrgScopes, error) {
	// UNSCOPED BY DESIGN — authority derivation, exactly as the library spells
	// it: this computes what the principal may do in orgID, so it cannot be
	// gated on a scope derived from what the principal may do.
	member, err := m.GetMemberWithRole(ctx, orgID, userID, roleReadScope())
	if errors.Is(err, idstore.ErrNotFound) {
		return idauth.OrgScopes{}, nil
	}
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(member.RoleTemplateScopes))
	for _, s := range member.RoleTemplateScopes {
		seen[s] = true
	}
	scopes := make(idauth.OrgScopes, 0, len(seen))
	for s := range seen {
		scopes = append(scopes, s)
	}
	return scopes, nil
}

// ListMembersWithUsers returns an organization's members with the role each
// carries here.
func (m *Members) ListMembersWithUsers(ctx context.Context, orgID string, scope idstore.OrgScope) ([]*idmodels.OrganizationMemberWithUser, error) {
	if m.store == nil {
		return nil, ErrNoAppStore
	}
	rows, err := m.identityOrgs.ListMembersWithUsers(ctx, orgID, scope)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return rows, nil
	}
	roles, err := m.store.RolesForOrganization(ctx, orgID, scope)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		if row == nil {
			continue
		}
		applyToMemberWithUser(row, roles[row.UserID])
	}
	return rows, nil
}

// GetMember returns one membership row, carrying the role id this application
// records rather than identity's.
func (m *Members) GetMember(ctx context.Context, orgID, userID string, scope idstore.OrgScope) (*idmodels.OrganizationMember, error) {
	if m.store == nil {
		return nil, ErrNoAppStore
	}
	row, err := m.identityOrgs.GetMember(ctx, orgID, userID, scope)
	if err != nil {
		return nil, err
	}
	role, _, err := m.store.RoleForPair(ctx, orgID, userID, scope)
	if err != nil {
		return nil, err
	}
	if row != nil {
		row.RoleTemplateID = role.TemplateID
	}
	return row, nil
}

// CheckMembership answers "is this principal a member here, and with what role".
//
// RE-IMPLEMENTED: the library derives it from its own GetMember. The boolean is
// membership, which is still identity's fact; the role id is this application's.
// The ErrNotFound absorption is the library's, kept verbatim — a lookup that
// FAILED must not be reported as "not a member".
func (m *Members) CheckMembership(ctx context.Context, orgID, userID string, scope idstore.OrgScope) (bool, *string, error) {
	member, err := m.GetMember(ctx, orgID, userID, scope)
	if errors.Is(err, idstore.ErrNotFound) {
		return false, nil, nil
	}
	if err != nil {
		return false, nil, err
	}
	return true, member.RoleTemplateID, nil
}

// ListMembers returns an organization's membership rows, carrying the role ids
// this application records.
func (m *Members) ListMembers(ctx context.Context, orgID string, scope idstore.OrgScope) ([]*idmodels.OrganizationMember, error) {
	if m.store == nil {
		return nil, ErrNoAppStore
	}
	rows, err := m.identityOrgs.ListMembers(ctx, orgID, scope)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return rows, nil
	}
	roles, err := m.store.RolesForOrganization(ctx, orgID, scope)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		if row == nil {
			continue
		}
		row.RoleTemplateID = roles[row.UserID].TemplateID
	}
	return rows, nil
}

// roleReadScope is the tenancy an AUTHORITY-DERIVING role read runs under.
//
// PLATFORM-WIDE, AND SPELLED RATHER THAN IMPLIED, so it appears in
// TestPlatformWideOrgScopeSitesAreReviewed's enumeration and has to be signed off
// there — the same treatment reconcileScope gets.
//
// It is correct for exactly two callers and no others. GetUserMemberships and
// GetUserScopesForOrg are the accessors that COMPUTE where a principal may act;
// the shared library marks both "UNSCOPED BY DESIGN" for the same reason, because
// gating them on a scope derived from the answer would be circular. Every other
// override here forwards the CALLER'S scope, and the rows the overlay decorates
// have already been filtered by the identity leg under that scope — so a widened
// overlay cannot disclose a membership the caller could not already see.
func roleReadScope() idstore.OrgScope { return idstore.OrgScopeAllOrganizations() }

// applyToUserMembership replaces a membership row's role fields with this
// application's answer.
//
// EVERY FIELD IS OVERWRITTEN, including with the zero value. A row that came back
// from identity carrying identity's role name and scopes must not keep any of
// them when the app tables have no role for that pair: a half-overlaid row is a
// principal shown one role and granted another, which is the exact failure this
// phase has to avoid. Scopes become the empty (non-nil) slice, matching the
// library's own COALESCE to '[]'.
func applyToUserMembership(row *idmodels.UserMembership, role Role) {
	row.RoleTemplateID = role.TemplateID
	row.RoleTemplateName = role.Name
	row.RoleTemplateDisplayName = role.DisplayName
	row.RoleTemplateScopes = nonNilScopes(role.Scopes)
}

// applyToMemberWithUser is applyToUserMembership for the org-member shape.
func applyToMemberWithUser(row *idmodels.OrganizationMemberWithUser, role Role) {
	if row == nil {
		return
	}
	row.RoleTemplateID = role.TemplateID
	row.RoleTemplateName = role.Name
	row.RoleTemplateDisplayName = role.DisplayName
	row.RoleTemplateScopes = nonNilScopes(role.Scopes)
}

// logNoAppStore announces a Members built without an application connection.
//
// Announced at construction rather than discovered at the first read: every
// role-carrying read on this repository returns ErrNoAppStore, and in a server
// that means no principal can be authorized on this path at all. The unit-test
// rigs that exercise only the identity leg's writes legitimately hold one; a
// server never should, and this line in the log is how that is seen, rather than
// inferred later from a wall of denials.
func logNoAppStore() {
	slog.Warn("role reads on this repository will be refused: it has no application database connection",
		"error", ErrNoAppStore.Error())
}
