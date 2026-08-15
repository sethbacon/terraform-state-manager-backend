package approles

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"sort"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	idstore "github.com/sethbacon/terraform-suite-identity/identity/store"
)

// These tests pin the SUBSTITUTION, not merely the plumbing.
//
// Every case below stages an identity leg that returns ONE role and an app leg
// that returns a DIFFERENT one, then asserts on the exact value that came back.
// Staging them to agree would pass identically whether the overlay ran or not,
// which is the shape that certified nothing in this repo's own four cross-tenant
// tests: they asserted on ExpectationsWereMet, which reports unmet expectations
// rather than a wrong answer, and passed with every fix reverted.

const (
	identityRoleID = "cccccccc-0000-0000-0000-000000000001"
	appRoleID      = "dddddddd-0000-0000-0000-000000000002"
	testOrgID      = "11111111-0000-0000-0000-00000000000a"
	testUserID     = "22222222-0000-0000-0000-00000000000b"
)

// readsEnv is a Members with both legs mocked.
type readsEnv struct {
	members       *Members
	identity, app sqlmock.Sqlmock
}

func newReadsEnv(t *testing.T, source RoleSource) *readsEnv {
	t.Helper()
	identityDB, identityMock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New (identity): %v", err)
	}
	appDB, appMock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New (app): %v", err)
	}
	t.Cleanup(func() {
		_ = identityDB.Close()
		_ = appDB.Close()
	})
	return &readsEnv{members: NewMembers(identityDB, appDB, source), identity: identityMock, app: appMock}
}

// expectIdentityMembership stages the shared repository's user-membership read,
// carrying IDENTITY's answer for the role.
func expectIdentityMembership(m sqlmock.Sqlmock, roleID, roleName string, scopes string) {
	m.ExpectQuery(`FROM organization_members om`).
		WithArgs(testUserID).
		WillReturnRows(sqlmock.NewRows([]string{
			"organization_id", "organization_name", "role_template_id", "created_at",
			"role_template_name", "role_template_display_name", "role_template_scopes",
		}).AddRow(testOrgID, "acme", roleID, time.Now(), roleName, roleName, []byte(scopes)))
}

// expectAppRolesForUser stages this application's answer for the same principal.
func expectAppRolesForUser(m sqlmock.Sqlmock, roleID, roleName, scopes string) {
	m.ExpectQuery(`FROM organization_member_roles r`).
		WithArgs(testUserID).
		WillReturnRows(sqlmock.NewRows([]string{"organization_id", "role_template_id", "name", "display_name", "scopes"}).
			AddRow(testOrgID, roleID, roleName, roleName, []byte(scopes)))
}

// THE ANSWER COMES FROM THIS APPLICATION'S TABLES. Identity says `viewer` with
// state:read; the mirror says `admin` with admin. The scopes a session would be
// minted from must be the mirror's.
func TestGetUserCombinedScopes_AnswersFromTheApplicationsOwnTables(t *testing.T) {
	env := newReadsEnv(t, RoleSourceApp)
	expectIdentityMembership(env.identity, identityRoleID, "viewer", `["state:read"]`)
	expectAppRolesForUser(env.app, appRoleID, "admin", `["admin"]`)

	scopes, err := env.members.GetUserCombinedScopes(context.Background(), testUserID)
	if err != nil {
		t.Fatalf("GetUserCombinedScopes: %v", err)
	}
	if len(scopes) != 1 || scopes[0] != "admin" {
		t.Fatalf("scopes = %v, want [admin] — identity's [state:read] means the read never left the shared schema", scopes)
	}
}

// THE ROLLBACK POSITION ANSWERS FROM IDENTITY, and issues no app query at all.
// Asserted on the value AND on the app mock having been asked nothing: a
// rollback that read the mirror and then discarded it would still be reading the
// table an operator rolled back to stop reading.
func TestGetUserCombinedScopes_RollbackAnswersFromIdentity(t *testing.T) {
	env := newReadsEnv(t, RoleSourceIdentity)
	expectIdentityMembership(env.identity, identityRoleID, "viewer", `["state:read"]`)
	// No app expectation: any query on that connection fails this test.

	scopes, err := env.members.GetUserCombinedScopes(context.Background(), testUserID)
	if err != nil {
		t.Fatalf("GetUserCombinedScopes: %v", err)
	}
	if len(scopes) != 1 || scopes[0] != "state:read" {
		t.Fatalf("scopes = %v, want [state:read]", scopes)
	}
	if env.members.Source() != RoleSourceIdentity {
		t.Fatalf("Source() = %q, want %q", env.members.Source(), RoleSourceIdentity)
	}
}

// A MEMBERSHIP THIS APPLICATION RECORDS NO ROLE FOR GRANTS NOTHING. This is the
// direction a gap in the mirror has to fail in: the principal loses access they
// should have (loud, and self-reporting) rather than keeping access they should
// not (silent). Every field is cleared, not just the scopes — a row left carrying
// identity's role NAME beside an empty scope set is a principal shown one role
// and granted another.
func TestGetUserMemberships_AMissingMirrorRowGrantsNothing(t *testing.T) {
	env := newReadsEnv(t, RoleSourceApp)
	expectIdentityMembership(env.identity, identityRoleID, "admin", `["admin"]`)
	env.app.ExpectQuery(`FROM organization_member_roles r`).
		WithArgs(testUserID).
		WillReturnRows(sqlmock.NewRows([]string{"organization_id", "role_template_id", "name", "display_name", "scopes"}))

	rows, err := env.members.GetUserMemberships(context.Background(), testUserID)
	if err != nil {
		t.Fatalf("GetUserMemberships: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d memberships, want 1: membership is still identity's fact", len(rows))
	}
	m := rows[0]
	if m.OrganizationID != testOrgID || m.OrganizationName != "acme" {
		t.Errorf("the membership fact was altered: %+v", m)
	}
	if m.RoleTemplateID != nil {
		t.Errorf("RoleTemplateID = %v, want nil", *m.RoleTemplateID)
	}
	if m.RoleTemplateName != nil {
		t.Errorf("RoleTemplateName = %q, want nil: identity's role name survived the overlay", *m.RoleTemplateName)
	}
	if m.RoleTemplateDisplayName != nil {
		t.Errorf("RoleTemplateDisplayName = %q, want nil", *m.RoleTemplateDisplayName)
	}
	if len(m.RoleTemplateScopes) != 0 {
		t.Errorf("RoleTemplateScopes = %v, want empty", m.RoleTemplateScopes)
	}
}

// OrgScopeForUser is DERIVED inside the library from its own GetUserMemberships,
// so a wrapper that overrode only the base read would leave every tenancy
// decision on identity's roles. Identity grants the required scope here and the
// mirror does not: the resolver must return the empty scope.
func TestOrgScopeForUser_ResolvesFromTheApplicationsOwnRoles(t *testing.T) {
	env := newReadsEnv(t, RoleSourceApp)
	expectIdentityMembership(env.identity, identityRoleID, "org_owner", `["organizations:write"]`)
	expectAppRolesForUser(env.app, appRoleID, "viewer", `["state:read"]`)

	scope, err := env.members.OrgScopeForUser(context.Background(), testUserID, "organizations:write", nil)
	if err != nil {
		t.Fatalf("OrgScopeForUser: %v", err)
	}
	if got := scope.OrganizationIDs(); len(got) != 0 {
		t.Fatalf("OrgScopeForUser = %v, want empty: this application's `viewer` does not grant organizations:write, "+
			"and only identity's copy of the membership says otherwise", got)
	}
	if !scope.MatchesNothing() {
		t.Error("the resolved scope does not deny: a caller holding it would reach every organization")
	}
}

// The same, in the direction that GRANTS: the mirror is what widens a scope, so
// a resolver still reading identity would under-authorize an administrator this
// application promoted.
func TestOrgScopeForUser_GrantsFromTheApplicationsOwnRoles(t *testing.T) {
	env := newReadsEnv(t, RoleSourceApp)
	expectIdentityMembership(env.identity, identityRoleID, "viewer", `["state:read"]`)
	expectAppRolesForUser(env.app, appRoleID, "org_owner", `["organizations:write"]`)

	scope, err := env.members.OrgScopeForUser(context.Background(), testUserID, "organizations:write", nil)
	if err != nil {
		t.Fatalf("OrgScopeForUser: %v", err)
	}
	got := scope.OrganizationIDs()
	if len(got) != 1 || got[0] != testOrgID {
		t.Fatalf("OrgScopeForUser = %v, want [%s]", got, testOrgID)
	}
}

// GetUserScopesForOrg is derived from GetMemberWithRole, which is derived from a
// different query shape (the org-member-with-user projection). Overriding one and
// not the other is the exact promotion trap this phase's class guard exists for,
// so it is asserted behaviourally too.
func TestGetUserScopesForOrg_AnswersFromTheApplicationsOwnTables(t *testing.T) {
	env := newReadsEnv(t, RoleSourceApp)
	env.identity.ExpectQuery(`FROM organization_members om`).
		WithArgs(testOrgID, testUserID).
		WillReturnRows(sqlmock.NewRows([]string{
			"organization_id", "user_id", "role_template_id", "created_at",
			"user_name", "user_email", "role_template_name", "role_template_display_name", "role_template_scopes",
		}).AddRow(testOrgID, testUserID, identityRoleID, time.Now(), "Alice", "alice@example.com",
			"admin", "Administrator", []byte(`["admin"]`)))
	env.app.ExpectQuery(`FROM organization_member_roles r`).
		WithArgs(testOrgID, testUserID).
		WillReturnRows(sqlmock.NewRows([]string{"role_template_id", "name", "display_name", "scopes"}).
			AddRow(appRoleID, "viewer", "Viewer", []byte(`["state:read"]`)))

	scopes, err := env.members.GetUserScopesForOrg(context.Background(), testUserID, testOrgID)
	if err != nil {
		t.Fatalf("GetUserScopesForOrg: %v", err)
	}
	if len(scopes) != 1 || scopes[0] != "state:read" {
		t.Fatalf("scopes = %v, want [state:read] — identity's [admin] means the per-organization decision "+
			"never left the shared schema", scopes)
	}
}

// CheckMembership is derived from GetMember. Membership stays identity's answer;
// the ROLE ID it hands back is this application's, and the two must not be mixed.
func TestCheckMembership_KeepsIdentitysMembershipAndTheApplicationsRole(t *testing.T) {
	env := newReadsEnv(t, RoleSourceApp)
	env.identity.ExpectQuery(`FROM organization_members`).
		WithArgs(testOrgID, testUserID, sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"organization_id", "user_id", "role_template_id", "created_at"}).
			AddRow(testOrgID, testUserID, identityRoleID, time.Now()))
	env.app.ExpectQuery(`FROM organization_member_roles r`).
		WithArgs(testOrgID, testUserID, sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"role_template_id", "name", "display_name", "scopes"}).
			AddRow(appRoleID, "viewer", "Viewer", []byte(`["state:read"]`)))

	isMember, roleID, err := env.members.CheckMembership(context.Background(), testOrgID, testUserID,
		idstore.OrgScopeOrganizations(testOrgID))
	if err != nil {
		t.Fatalf("CheckMembership: %v", err)
	}
	if !isMember {
		t.Fatal("isMember = false: membership is identity's fact and identity has the row")
	}
	if roleID == nil || *roleID != appRoleID {
		t.Fatalf("role id = %v, want %s — the promoted CheckMembership returns identity's", roleID, appRoleID)
	}
}

// A member identity does not have is not a member here either, and the app leg
// must not be consulted for one: the overlay decorates rows identity returned, so
// "not a member" short-circuits before any app query.
func TestCheckMembership_AbsentInIdentityIsNotAMember(t *testing.T) {
	env := newReadsEnv(t, RoleSourceApp)
	env.identity.ExpectQuery(`FROM organization_members`).
		WithArgs(testOrgID, testUserID, sqlmock.AnyArg()).
		WillReturnError(sql.ErrNoRows)
	// No app expectation.

	isMember, roleID, err := env.members.CheckMembership(context.Background(), testOrgID, testUserID,
		idstore.OrgScopeOrganizations(testOrgID))
	if err != nil {
		t.Fatalf("CheckMembership: %v", err)
	}
	if isMember || roleID != nil {
		t.Fatalf("CheckMembership = (%v, %v), want (false, nil)", isMember, roleID)
	}
}

// ListMembersWithUsers overlays by USER within one organization, which is the
// other keying and therefore the other place an overlay can be wired to the wrong
// column. Two members, two different roles, deliberately in the opposite order
// from the identity rows.
func TestListMembersWithUsers_OverlaysEachMemberSeparately(t *testing.T) {
	env := newReadsEnv(t, RoleSourceApp)
	const otherUser = "22222222-0000-0000-0000-00000000000c"
	env.identity.ExpectQuery(`FROM organization_members om`).
		WithArgs(testOrgID, sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{
			"organization_id", "user_id", "role_template_id", "created_at",
			"user_name", "user_email", "role_template_name", "role_template_display_name", "role_template_scopes",
		}).
			AddRow(testOrgID, testUserID, identityRoleID, time.Now(), "Alice", "a@example.com", "admin", "Administrator", []byte(`["admin"]`)).
			AddRow(testOrgID, otherUser, identityRoleID, time.Now(), "Bob", "b@example.com", "admin", "Administrator", []byte(`["admin"]`)))
	env.app.ExpectQuery(`FROM organization_member_roles r`).
		WithArgs(testOrgID, sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"user_id", "role_template_id", "name", "display_name", "scopes"}).
			AddRow(otherUser, appRoleID, "editor", "Editor", []byte(`["state:write"]`)).
			AddRow(testUserID, appRoleID, "viewer", "Viewer", []byte(`["state:read"]`)))

	members, err := env.members.ListMembersWithUsers(context.Background(), testOrgID, idstore.OrgScopeOrganizations(testOrgID))
	if err != nil {
		t.Fatalf("ListMembersWithUsers: %v", err)
	}
	got := map[string]string{}
	for _, m := range members {
		if m.RoleTemplateName == nil {
			t.Fatalf("member %s lost its role name entirely", m.UserID)
		}
		got[m.UserID] = *m.RoleTemplateName
	}
	if got[testUserID] != "viewer" || got[otherUser] != "editor" {
		t.Fatalf("roles = %v, want %s->viewer and %s->editor; identity said admin for both, "+
			"so anything else means the overlay keyed on the wrong column", got, testUserID, otherUser)
	}
}

// AN UNDECIDED ROLE SOURCE DENIES. The zero RoleSource is what a construction
// site that never thought about it holds, and there is no safe guess: defaulting
// to identity would silently undo this phase on that path, and defaulting to app
// would silently perform it on a path with no app tables. So it errors, with the
// value in the message.
func TestAnUndecidedRoleSourceIsRefused(t *testing.T) {
	env := newReadsEnv(t, RoleSource(""))
	expectIdentityMembership(env.identity, identityRoleID, "admin", `["admin"]`)

	_, err := env.members.GetUserCombinedScopes(context.Background(), testUserID)
	if !errors.Is(err, ErrNoRoleSource) {
		t.Fatalf("GetUserCombinedScopes with no role source: got %v, want ErrNoRoleSource", err)
	}
}

// A Members with NO app connection degrades to identity rather than answering
// nothing, and says so through Source(). That is what keeps the unit-test rigs
// and the constructions that predate an app connection working; in a server it
// would mean this phase is not in effect on that path.
func TestNoApplicationConnectionDegradesToIdentity(t *testing.T) {
	identityDB, identityMock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = identityDB.Close() }()

	m := NewMembers(identityDB, nil, RoleSourceApp)
	if m.Source() != RoleSourceIdentity {
		t.Fatalf("Source() = %q with no app connection, want %q", m.Source(), RoleSourceIdentity)
	}
	expectIdentityMembership(identityMock, identityRoleID, "viewer", `["state:read"]`)
	scopes, err := m.GetUserCombinedScopes(context.Background(), testUserID)
	if err != nil {
		t.Fatalf("GetUserCombinedScopes: %v", err)
	}
	if len(scopes) != 1 || scopes[0] != "state:read" {
		t.Fatalf("scopes = %v, want [state:read]", scopes)
	}
}

// ParseRoleSource accepts exactly the two positions and refuses everything else,
// including the empty string. Empty is refused rather than treated as "the
// default" because config supplies the default: accepting it here would make a
// mis-spelled key indistinguishable from an unset one at the layer that can no
// longer tell.
func TestParseRoleSource(t *testing.T) {
	cases := []struct {
		in      string
		want    RoleSource
		wantErr bool
	}{
		{"app", RoleSourceApp, false},
		{"APP", RoleSourceApp, false},
		{"  identity  ", RoleSourceIdentity, false},
		{"identity", RoleSourceIdentity, false},
		{"", "", true},
		{"idenity", "", true},
		{"shared", "", true},
	}
	for _, c := range cases {
		got, err := ParseRoleSource(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("ParseRoleSource(%q) = %q, want an error", c.in, got)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("ParseRoleSource(%q) = (%q, %v), want (%q, nil)", c.in, got, err, c.want)
		}
	}
}

// The app-side role reads bind the caller's tenancy INTO the statement, so a
// scope nobody decided matches nothing rather than everything. Asserted on the
// rendered SQL, because the zero OrgScope and the platform-wide one differ by one
// literal and produce identical-looking Go.
func TestAppRoleReadsBindTheCallersTenancy(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()
	store := NewStore(db)

	// The zero OrgScope is what a caller who has not decided holds.
	mock.ExpectQuery(`AND FALSE`).
		WithArgs(testUserID).
		WillReturnRows(sqlmock.NewRows([]string{"organization_id", "role_template_id", "name", "display_name", "scopes"}))
	if _, err := store.RolesForUser(context.Background(), testUserID, idstore.OrgScope{}); err != nil {
		t.Fatalf("RolesForUser with the zero scope: %v", err)
	}

	mock.ExpectQuery(`AND TRUE`).
		WithArgs(testUserID).
		WillReturnRows(sqlmock.NewRows([]string{"organization_id", "role_template_id", "name", "display_name", "scopes"}))
	if _, err := store.RolesForUser(context.Background(), testUserID, idstore.OrgScopeAllOrganizations()); err != nil {
		t.Fatalf("RolesForUser platform-wide: %v", err)
	}

	mock.ExpectQuery(regexp.QuoteMeta(`r.organization_id = ANY(`)).
		WithArgs(testOrgID, sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"user_id", "role_template_id", "name", "display_name", "scopes"}))
	if _, err := store.RolesForOrganization(context.Background(), testOrgID, idstore.OrgScopeOrganizations(testOrgID)); err != nil {
		t.Fatalf("RolesForOrganization scoped: %v", err)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

// RoleForPair distinguishes "no row" from "a row with a NULL role". Both deny
// identically in the read path, and collapsing them here would leave the drift
// comparison and the read path disagreeing about what they are looking at.
func TestRoleForPair_DistinguishesNoRowFromANullRole(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()
	store := NewStore(db)
	scope := idstore.OrgScopeOrganizations(testOrgID)

	mock.ExpectQuery(`FROM organization_member_roles r`).
		WillReturnRows(sqlmock.NewRows([]string{"role_template_id", "name", "display_name", "scopes"}))
	role, present, err := store.RoleForPair(context.Background(), testOrgID, testUserID, scope)
	if err != nil {
		t.Fatalf("RoleForPair (no row): %v", err)
	}
	if present || role.TemplateID != nil {
		t.Fatalf("no row reported as present=%v role=%+v", present, role)
	}

	mock.ExpectQuery(`FROM organization_member_roles r`).
		WillReturnRows(sqlmock.NewRows([]string{"role_template_id", "name", "display_name", "scopes"}).
			AddRow(nil, nil, nil, []byte(`[]`)))
	role, present, err = store.RoleForPair(context.Background(), testOrgID, testUserID, scope)
	if err != nil {
		t.Fatalf("RoleForPair (null role): %v", err)
	}
	if !present {
		t.Fatal("a row with a NULL role was reported as absent")
	}
	if role.TemplateID != nil || len(role.Scopes) != 0 {
		t.Fatalf("role = %+v, want a present row granting nothing", role)
	}
}

// Scope sets are compared as sets everywhere else in this package; the read path
// deduplicates for the same reason, so a template that lists a scope twice does
// not produce a session carrying it twice.
func TestGetUserCombinedScopes_Deduplicates(t *testing.T) {
	env := newReadsEnv(t, RoleSourceApp)
	env.identity.ExpectQuery(`FROM organization_members om`).
		WithArgs(testUserID).
		WillReturnRows(sqlmock.NewRows([]string{
			"organization_id", "organization_name", "role_template_id", "created_at",
			"role_template_name", "role_template_display_name", "role_template_scopes",
		}).
			AddRow(testOrgID, "acme", identityRoleID, time.Now(), "viewer", "Viewer", []byte(`["state:read"]`)).
			AddRow("11111111-0000-0000-0000-00000000000f", "other", identityRoleID, time.Now(), "viewer", "Viewer", []byte(`["state:read"]`)))
	env.app.ExpectQuery(`FROM organization_member_roles r`).
		WithArgs(testUserID).
		WillReturnRows(sqlmock.NewRows([]string{"organization_id", "role_template_id", "name", "display_name", "scopes"}).
			AddRow(testOrgID, appRoleID, "editor", "Editor", []byte(`["state:read","state:write"]`)).
			AddRow("11111111-0000-0000-0000-00000000000f", appRoleID, "editor", "Editor", []byte(`["state:write"]`)))

	scopes, err := env.members.GetUserCombinedScopes(context.Background(), testUserID)
	if err != nil {
		t.Fatalf("GetUserCombinedScopes: %v", err)
	}
	got := append([]string(nil), scopes...)
	sort.Strings(got)
	if len(got) != 2 || got[0] != "state:read" || got[1] != "state:write" {
		t.Fatalf("scopes = %v, want [state:read state:write]", got)
	}
}
