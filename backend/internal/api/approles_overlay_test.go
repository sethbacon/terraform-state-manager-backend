package api

import (
	"database/sql/driver"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
)

// The app-side overlay every role-carrying read issues after its identity leg.
//
// Since sethbacon/terraform-state-manager-backend#599 the role a principal holds
// comes from THIS application's tables and nowhere else: approles.Members reads
// identity for the membership FACT, then overlays the role from
// organization_member_roles joined to its own role_templates, and a Members with
// no application connection refuses the read outright. So a rig that stages an
// identity membership row carrying a role must stage the same role here, in
// order, or the handler under test sees a principal with no role at all — the
// fail-closed direction, and the wrong test. The identity row's role columns are
// still scanned by the shared library and then overwritten by this answer.
//
// Three shapes, matching approles.Store:
//
//   - RolesForUser (GetUserMemberships and everything derived from it — the
//     scope union, OrgScopeForUser): keyed by organization, args (userID).
//   - RoleForPair (GetMemberWithRole, GetMember, CheckMembership,
//     GetUserScopesForOrg): one row or none, args (orgID, userID), plus the
//     caller's scope argument when that read is scoped to named organizations.
//   - RolesForOrganization (ListMembersWithUsers, ListMembers): keyed by user,
//     args (orgID), plus the caller's scope argument likewise.
//
// All three start `SELECT r.… FROM organization_member_roles r`, so the pattern
// is shared and in-order matching plus the argument list tells them apart. An
// EMPTY identity read stages nothing: every list-shaped override short-circuits
// before the overlay when there are no rows to decorate, and a single-row read
// that found nothing has already returned ErrNotFound.

// appRole is one row of the overlay: the role this application records for one
// (organization, user) pair. key is the organization for RolesForUser and the
// user for RolesForOrganization; RoleForPair has no key.
type appRole struct {
	key, id, name, scopes string
}

// expectAppRolesForUser stages RolesForUser for userID, one row per role, keyed
// by organization.
func expectAppRolesForUser(mock sqlmock.Sqlmock, userID string, roles ...appRole) {
	rows := sqlmock.NewRows([]string{"organization_id", "role_template_id", "name", "display_name", "scopes"})
	for _, r := range roles {
		rows.AddRow(r.key, r.id, r.name, r.name, []byte(r.scopes))
	}
	mock.ExpectQuery("FROM organization_member_roles r").WithArgs(userID).WillReturnRows(rows)
}

// expectAppRolesForOrganization stages RolesForOrganization for orgID, one row
// per role, keyed by user. scopeArgs carries the caller's scope argument when
// the read was scoped to named organizations rather than platform-wide.
func expectAppRolesForOrganization(mock sqlmock.Sqlmock, orgID string, roles []appRole, scopeArgs ...driver.Value) {
	rows := sqlmock.NewRows([]string{"user_id", "role_template_id", "name", "display_name", "scopes"})
	for _, r := range roles {
		rows.AddRow(r.key, r.id, r.name, r.name, []byte(r.scopes))
	}
	mock.ExpectQuery("FROM organization_member_roles r").WithArgs(append([]driver.Value{orgID}, scopeArgs...)...).WillReturnRows(rows)
}

// expectAppRoleForPair stages RoleForPair for (orgID, userID). A nil role stages
// no row: a member this application records no role for, which is still a
// member — the boolean is identity's fact, only the role is ours.
func expectAppRoleForPair(mock sqlmock.Sqlmock, orgID, userID string, role *appRole, scopeArgs ...driver.Value) {
	rows := sqlmock.NewRows([]string{"role_template_id", "name", "display_name", "scopes"})
	if role != nil {
		rows.AddRow(role.id, role.name, role.name, []byte(role.scopes))
	}
	mock.ExpectQuery("FROM organization_member_roles r").WithArgs(append([]driver.Value{orgID, userID}, scopeArgs...)...).WillReturnRows(rows)
}
