package approles

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

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
// and what the gate in cmd/server (authz-drift) must show zero of before a
// deployment is upgraded onto this build.
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

// RoleSource names the tables an authorization read resolves a role from.
//
// It is the rollback lever. Phase 3a's dual write is unchanged by this phase, so
// identity.organization_members still carries every current role assignment and
// identity.role_templates still carries a role definition for every name; an
// operator who finds the flip wrong sets this back to `identity`, restarts, and
// is running Phase 3a's behaviour exactly, with no migration, no data movement and
// no window. See docs/adr/006-per-app-authorization-reads.md.
type RoleSource string

const (
	// RoleSourceApp resolves roles from THIS application's own tables. The
	// Phase 3b default.
	RoleSourceApp RoleSource = "app"
	// RoleSourceIdentity resolves roles from the shared identity schema, as
	// Phase 3a did. The rollback position.
	RoleSourceIdentity RoleSource = "identity"
)

// ErrNoRoleSource reports a Members whose role source was never decided.
//
// A SENTINEL AND A DENIAL, not a default. The zero RoleSource is the value a
// construction site that has not thought about it holds, and there is no safe
// guess: defaulting to identity would silently un-do this phase on that path,
// and defaulting to app would silently perform it on a path with no app tables.
// So it resolves to nothing and says so, in the same shape reduceAuthority
// refuses a nil AuthorityReducer.
var ErrNoRoleSource = errors.New("approles: no role source was configured for this repository")

// ParseRoleSource converts an operator's configured value into a RoleSource.
//
// Empty is NOT accepted as "the default". Config supplies the default (see
// internal/config), and accepting empty here would make a mis-spelled key
// indistinguishable from an unset one at the layer that can no longer tell.
func ParseRoleSource(v string) (RoleSource, error) {
	switch RoleSource(strings.ToLower(strings.TrimSpace(v))) {
	case RoleSourceApp:
		return RoleSourceApp, nil
	case RoleSourceIdentity:
		return RoleSourceIdentity, nil
	default:
		return "", fmt.Errorf("approles: unknown role source %q (want %q or %q)", v, RoleSourceApp, RoleSourceIdentity)
	}
}

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

// source returns the role source this repository actually reads from.
//
// A Members with no app connection has no tables to read, so it reports
// identity regardless of what was asked for. That degradation is what keeps the
// unit-test rigs and the handful of constructions that predate an app connection
// working; it is announced at construction (NewMembers) rather than discovered,
// because in a server it would mean this phase is not in effect at all.
func (m *Members) source() RoleSource {
	if m.store == nil {
		return RoleSourceIdentity
	}
	return m.roleSource
}

// Source reports the role source in effect, for the startup line and for tests.
func (m *Members) Source() RoleSource { return m.source() }

// GetUserMemberships returns a user's memberships with the role each carries in
// THIS application.
//
// UNSCOPED, as the shared library's is, and for its reason: this is authority
// derivation and a scope parameter would ask the caller for the answer. The
// overlay reads the whole of one user's row set from the app tables, so the
// platform-wide scope is spelled — see roleReadScope.
func (m *Members) GetUserMemberships(ctx context.Context, userID string) ([]*idmodels.UserMembership, error) {
	rows, err := m.identityOrgs.GetUserMemberships(ctx, userID)
	if err != nil || m.source() == RoleSourceIdentity {
		return rows, err
	}
	if m.source() != RoleSourceApp {
		return nil, unsetSource(m.roleSource)
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
	row, err := m.identityOrgs.GetMemberWithRole(ctx, orgID, userID, scope)
	if err != nil || m.source() == RoleSourceIdentity {
		return row, err
	}
	if m.source() != RoleSourceApp {
		return nil, unsetSource(m.roleSource)
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
	rows, err := m.identityOrgs.ListMembersWithUsers(ctx, orgID, scope)
	if err != nil || m.source() == RoleSourceIdentity {
		return rows, err
	}
	if m.source() != RoleSourceApp {
		return nil, unsetSource(m.roleSource)
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
	row, err := m.identityOrgs.GetMember(ctx, orgID, userID, scope)
	if err != nil || m.source() == RoleSourceIdentity {
		return row, err
	}
	if m.source() != RoleSourceApp {
		return nil, unsetSource(m.roleSource)
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
	rows, err := m.identityOrgs.ListMembers(ctx, orgID, scope)
	if err != nil || m.source() == RoleSourceIdentity {
		return rows, err
	}
	if m.source() != RoleSourceApp {
		return nil, unsetSource(m.roleSource)
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

// unsetSource turns an undecided role source into a denial with the value in it.
func unsetSource(s RoleSource) error {
	return fmt.Errorf("%w: %q is not %q or %q", ErrNoRoleSource, s, RoleSourceApp, RoleSourceIdentity)
}

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

// logDegradedSource announces a Members that was asked to read this
// application's tables and has no connection to them.
//
// Announced rather than refused: the unit-test rigs and the constructions that
// predate an app connection legitimately hold one, and refusing would turn a
// documented degradation into a nil pointer somewhere less obvious. In a server
// it means Phase 3b is not in effect on that path, which is a thing to see in the
// log rather than infer from behaviour.
func logDegradedSource(want RoleSource) {
	if want != RoleSourceApp {
		return
	}
	slog.Warn("role reads fall back to the shared identity schema: this repository has no application database connection",
		"requested_source", string(RoleSourceApp), "effective_source", string(RoleSourceIdentity))
}
