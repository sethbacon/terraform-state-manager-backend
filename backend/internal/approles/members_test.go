package approles

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	idstore "github.com/sethbacon/terraform-suite-identity/identity/store"
)

// These assert the two things a behavioural test in Phase 3a structurally
// cannot: that BOTH connections are written, and that they are written in the
// order the ordering rule prescribes.
//
// sqlmock is ORDERED by default, which is what makes the second assertion
// possible at all: expecting the mirror's DELETE before identity's fails if the
// implementation swaps them. That ordering is not cosmetic — it is the rule that
// a crash between the two legs leaves the LESS privileged state, and it is the
// only property of this phase that the phase which starts reading these tables
// does not get to re-choose.

const templateID = "11111111-1111-1111-1111-111111111111"

// tape records the order in which statements were matched ACROSS BOTH MOCKS.
//
// sqlmock's ordered expectations order statements WITHIN one mock, and the two
// legs here are deliberately two different mocks so that neither can satisfy the
// other's assertion. That means sqlmock alone cannot see the ordering rule at
// all: swapping identity-first for mirror-first leaves each mock's own sequence
// intact and every such test passing. This is the mechanism that closes that
// gap — a custom sqlmock.Argument records its label when the expectation it is
// attached to is matched, so the two mocks write into one sequence.
type tape struct{ steps []string }

// at returns an argument matcher that stamps label onto the tape. It matches
// anything; recording is the whole job.
func (tp *tape) at(label string) sqlmock.Argument { return tapeStep{tp: tp, label: label} }

type tapeStep struct {
	tp    *tape
	label string
}

// Match records and always accepts. Consecutive duplicates are collapsed:
// sqlmock may evaluate an argument more than once while matching a statement,
// and a doubled entry would be an artefact of the matcher rather than of the
// code under test.
func (s tapeStep) Match(driver.Value) bool {
	if n := len(s.tp.steps); n == 0 || s.tp.steps[n-1] != s.label {
		s.tp.steps = append(s.tp.steps, s.label)
	}
	return true
}

// wants asserts the recorded sequence, and refuses an EMPTY tape: a matcher that
// never fired records nothing, and "nothing, in the right order" is how an
// ordering assertion passes without having observed anything.
func (tp *tape) wants(t *testing.T, want ...string) {
	t.Helper()
	if len(tp.steps) == 0 {
		t.Fatal("the ordering tape recorded nothing: no expectation carrying a tape step was matched")
	}
	if len(tp.steps) != len(want) {
		t.Fatalf("statement order = %v, want %v", tp.steps, want)
	}
	for i := range want {
		if tp.steps[i] != want[i] {
			t.Fatalf("statement order = %v, want %v", tp.steps, want)
		}
	}
}

// twoConnections builds a Members whose identity leg and app leg are separate
// mocks, so an assertion on one cannot be satisfied by the other.
func twoConnections(t *testing.T) (*Members, sqlmock.Sqlmock, sqlmock.Sqlmock, func()) {
	t.Helper()
	identityDB, identityMock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New (identity): %v", err)
	}
	appDB, appMock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New (app): %v", err)
	}
	m := NewMembers(identityDB, appDB)
	return m, identityMock, appMock, func() {
		_ = identityDB.Close()
		_ = appDB.Close()
	}
}

func expectIdentityRoleLookup(mock sqlmock.Sqlmock, name string) {
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id FROM role_templates WHERE name = $1`)).
		WithArgs(name).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(templateID))
}

func TestAddMemberWithParams_WritesIdentityThenTheMirror(t *testing.T) {
	m, identityMock, appMock, done := twoConnections(t)
	defer done()

	var tp tape
	expectIdentityRoleLookup(identityMock, "editor")
	identityMock.ExpectExec("INSERT INTO organization_members").
		WithArgs(tp.at("identity"), "user-1", templateID, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	// The mirror resolves the name against TSM'S OWN table, not identity's.
	appMock.ExpectQuery(regexp.QuoteMeta(`SELECT id FROM role_templates WHERE name = $1`)).
		WithArgs("editor").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(templateID))
	appMock.ExpectExec("INSERT INTO organization_member_roles").
		WithArgs(tp.at("mirror"), "user-1", templateID).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := m.AddMemberWithParams(context.Background(), "org-1", "user-1", "editor", idstore.OrgScopeOrganizations("org-1")); err != nil {
		t.Fatalf("AddMemberWithParams: %v", err)
	}
	// A GRANT is identity-first, the opposite of a revocation: see the ordering
	// rule on approles.Members.
	tp.wants(t, "identity", "mirror")
	if err := identityMock.ExpectationsWereMet(); err != nil {
		t.Errorf("identity leg: %v", err)
	}
	if err := appMock.ExpectationsWereMet(); err != nil {
		t.Errorf("mirror leg: %v", err)
	}
}

// A grant whose identity leg FAILED must not be mirrored: the mirror would then
// be the only place the authority exists.
func TestAddMemberWithParams_IdentityFailureLeavesTheMirrorUntouched(t *testing.T) {
	m, identityMock, appMock, done := twoConnections(t)
	defer done()

	expectIdentityRoleLookup(identityMock, "editor")
	identityMock.ExpectExec("INSERT INTO organization_members").
		WillReturnError(errors.New("insert failed"))
	// appMock has NO expectations: any statement at all fails the test.

	err := m.AddMemberWithParams(context.Background(), "org-1", "user-1", "editor", idstore.OrgScopeOrganizations("org-1"))
	if err == nil {
		t.Fatal("AddMemberWithParams reported success despite a failed identity write")
	}
	if !strings.Contains(err.Error(), "insert failed") {
		t.Fatalf("the identity error was not returned verbatim: %v", err)
	}
	if err := appMock.ExpectationsWereMet(); err != nil {
		t.Errorf("mirror leg: %v", err)
	}
}

func TestAddMemberWithRoleTemplate_MirrorsTheSameTemplateID(t *testing.T) {
	m, identityMock, appMock, done := twoConnections(t)
	defer done()

	roleID := templateID
	identityMock.ExpectExec("INSERT INTO organization_members").
		WillReturnResult(sqlmock.NewResult(0, 1))
	// The id carried over from identity is already in TSM's own table, so no
	// template fetch is needed.
	appMock.ExpectQuery(regexp.QuoteMeta(`SELECT 1 FROM role_templates WHERE id = $1`)).
		WithArgs(roleID).
		WillReturnRows(sqlmock.NewRows([]string{"?column?"}).AddRow(1))
	appMock.ExpectExec("INSERT INTO organization_member_roles").
		WithArgs("org-1", "user-1", roleID).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := m.AddMemberWithRoleTemplate(context.Background(), "org-1", "user-1", &roleID, idstore.OrgScopeOrganizations("org-1")); err != nil {
		t.Fatalf("AddMemberWithRoleTemplate: %v", err)
	}
	if err := appMock.ExpectationsWereMet(); err != nil {
		t.Errorf("mirror leg: %v", err)
	}
}

// A NULL role is a state identity can represent (organization_members.role_template_id
// is nullable, and AddMemberWithRoleTemplate takes a *string), so the mirror has
// to represent it too rather than skipping the row.
func TestAddMemberWithRoleTemplate_NilRoleIsMirroredAsNull(t *testing.T) {
	m, identityMock, appMock, done := twoConnections(t)
	defer done()

	identityMock.ExpectExec("INSERT INTO organization_members").
		WillReturnResult(sqlmock.NewResult(0, 1))
	appMock.ExpectExec("INSERT INTO organization_member_roles").
		WithArgs("org-1", "user-1", nil).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := m.AddMemberWithRoleTemplate(context.Background(), "org-1", "user-1", nil, idstore.OrgScopeOrganizations("org-1")); err != nil {
		t.Fatalf("AddMemberWithRoleTemplate: %v", err)
	}
	if err := appMock.ExpectationsWereMet(); err != nil {
		t.Errorf("mirror leg: %v", err)
	}
}

func TestUpdateMemberRole_WritesBothSides(t *testing.T) {
	m, identityMock, appMock, done := twoConnections(t)
	defer done()

	expectIdentityRoleLookup(identityMock, "viewer")
	identityMock.ExpectExec("UPDATE organization_members").
		WillReturnResult(sqlmock.NewResult(0, 1))
	appMock.ExpectQuery(regexp.QuoteMeta(`SELECT id FROM role_templates WHERE name = $1`)).
		WithArgs("viewer").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(templateID))
	appMock.ExpectExec("INSERT INTO organization_member_roles").
		WithArgs("org-1", "user-1", templateID).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := m.UpdateMemberRole(context.Background(), "org-1", "user-1", "viewer", idstore.OrgScopeOrganizations("org-1")); err != nil {
		t.Fatalf("UpdateMemberRole: %v", err)
	}
	if err := identityMock.ExpectationsWereMet(); err != nil {
		t.Errorf("identity leg: %v", err)
	}
	if err := appMock.ExpectationsWereMet(); err != nil {
		t.Errorf("mirror leg: %v", err)
	}
}

// THE ORDERING RULE, on the revocation side. The mirror's DELETE is expected
// FIRST; sqlmock's ordered expectations fail if the implementation writes
// identity first, which is the ordering that would leave a revoked role still
// recorded here after a crash between the legs.
func TestRemoveMember_DeletesTheMirrorBeforeIdentity(t *testing.T) {
	m, identityMock, appMock, done := twoConnections(t)
	defer done()

	var tp tape
	appMock.ExpectExec(regexp.QuoteMeta(`DELETE FROM organization_member_roles WHERE organization_id = $1 AND user_id = $2`)).
		WithArgs(tp.at("mirror"), "user-1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	identityMock.ExpectExec("DELETE FROM organization_members").
		WithArgs(tp.at("identity"), "user-1", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := m.RemoveMember(context.Background(), "org-1", "user-1", idstore.OrgScopeOrganizations("org-1")); err != nil {
		t.Fatalf("RemoveMember: %v", err)
	}
	tp.wants(t, "mirror", "identity")
	if err := appMock.ExpectationsWereMet(); err != nil {
		t.Errorf("mirror leg: %v", err)
	}
	if err := identityMock.ExpectationsWereMet(); err != nil {
		t.Errorf("identity leg: %v", err)
	}
}

// ErrNotFound from identity means the membership was ALREADY absent — the end
// state this call asked for. The mirror must still be cleared, or the record the
// caller asked to be rid of survives.
func TestRemoveMember_ClearsTheMirrorEvenWhenIdentityHasNothingToRemove(t *testing.T) {
	m, identityMock, appMock, done := twoConnections(t)
	defer done()

	appMock.ExpectExec("DELETE FROM organization_member_roles").
		WithArgs("org-1", "user-1").
		WillReturnResult(sqlmock.NewResult(0, 0))
	identityMock.ExpectExec("DELETE FROM organization_members").
		WillReturnResult(sqlmock.NewResult(0, 0))

	err := m.RemoveMember(context.Background(), "org-1", "user-1", idstore.OrgScopeOrganizations("org-1"))
	if !errors.Is(err, idstore.ErrNotFound) {
		t.Fatalf("expected the shared store's ErrNotFound to be returned unchanged, got %v", err)
	}
	if err := appMock.ExpectationsWereMet(); err != nil {
		t.Errorf("mirror leg: %v", err)
	}
}

func TestDelete_RemovesTheOrganizationsMirroredRolesFirst(t *testing.T) {
	m, identityMock, appMock, done := twoConnections(t)
	defer done()

	var tp tape
	appMock.ExpectExec(regexp.QuoteMeta(`DELETE FROM organization_member_roles WHERE organization_id = $1`)).
		WithArgs(tp.at("mirror")).
		WillReturnResult(sqlmock.NewResult(0, 3))
	identityMock.ExpectExec("DELETE FROM organizations").
		WithArgs(tp.at("identity"), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := m.Delete(context.Background(), "org-1", idstore.OrgScopeOrganizations("org-1")); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	tp.wants(t, "mirror", "identity")
	if err := appMock.ExpectationsWereMet(); err != nil {
		t.Errorf("mirror leg: %v", err)
	}
}

// The platform-wide strip must reach EVERY organization in the mirror, and the
// post-pass must narrow to what identity says it actually emptied.
func TestRemoveAllMembershipsForUser_StripsBeforeAndAfter(t *testing.T) {
	m, identityMock, appMock, done := twoConnections(t)
	defer done()

	// Pre-pass: the scope is platform-wide, so no organization predicate.
	appMock.ExpectExec(regexp.QuoteMeta(`DELETE FROM organization_member_roles WHERE user_id = $1`)).
		WithArgs("user-1").
		WillReturnResult(sqlmock.NewResult(0, 2))
	identityMock.ExpectQuery("DELETE FROM organization_members").
		WillReturnRows(sqlmock.NewRows([]string{"organization_id"}).AddRow("org-1").AddRow("org-2"))
	// Post-pass: exactly the organizations identity reports it emptied.
	appMock.ExpectExec(regexp.QuoteMeta(`DELETE FROM organization_member_roles WHERE user_id = $1 AND organization_id = ANY($2)`)).
		WithArgs("user-1", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 0))

	removed, err := m.RemoveAllMembershipsForUser(context.Background(), "user-1", idstore.OrgScopeAllOrganizations())
	if err != nil {
		t.Fatalf("RemoveAllMembershipsForUser: %v", err)
	}
	if got := removed.OrganizationIDs(); len(got) != 2 {
		t.Fatalf("expected the emptied organizations to be reported unchanged, got %v", got)
	}
	if err := appMock.ExpectationsWereMet(); err != nil {
		t.Errorf("mirror leg: %v", err)
	}
}

// A scope that matches nothing must remove nothing from the mirror. Collapsing
// "no organizations" into "every organization" here would turn a denied request
// into a platform-wide strip.
func TestScopeOrganizations_DistinguishesEverythingFromNothing(t *testing.T) {
	if got := scopeOrganizations(idstore.OrgScopeAllOrganizations()); got != nil {
		t.Fatalf("the platform-wide scope must map to nil (every organization), got %v", got)
	}
	got := scopeOrganizations(idstore.OrgScope{})
	if got == nil {
		t.Fatal("the deny-everything scope mapped to nil, which DeleteRolesForUser reads as every organization")
	}
	if len(got) != 0 {
		t.Fatalf("the deny-everything scope must map to an empty allowlist, got %v", got)
	}
	one := scopeOrganizations(idstore.OrgScopeOrganizations("org-9"))
	if len(one) != 1 || one[0] != "org-9" {
		t.Fatalf("an allowlist scope must map to its ids, got %v", one)
	}
}

// PurgeUserRoles is the pairing the class guard requires at every DeleteUser
// call site, so it must actually strip every organization.
func TestPurgeUserRoles_StripsEveryOrganization(t *testing.T) {
	m, _, appMock, done := twoConnections(t)
	defer done()

	appMock.ExpectExec(regexp.QuoteMeta(`DELETE FROM organization_member_roles WHERE user_id = $1`)).
		WithArgs("user-1").
		WillReturnResult(sqlmock.NewResult(0, 2))

	m.PurgeUserRoles(context.Background(), "user-1")
	if err := appMock.ExpectationsWereMet(); err != nil {
		t.Errorf("mirror leg: %v", err)
	}
}

// Constructed without an app connection, the mirror is absent and every write
// degrades to the identity leg alone rather than panicking. That is the unit-test
// rig's shape, and it must not be the server's.
func TestNoAppConnection_DegradesToTheIdentityLegAlone(t *testing.T) {
	identityDB, identityMock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = identityDB.Close() }()

	m := NewMembers(identityDB, nil)
	if m.Store() != nil {
		t.Fatal("a Members built without an app connection reported a mirror store")
	}
	identityMock.ExpectExec("DELETE FROM organization_members").
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := m.RemoveMember(context.Background(), "org-1", "user-1", idstore.OrgScopeOrganizations("org-1")); err != nil {
		t.Fatalf("RemoveMember: %v", err)
	}
	if err := identityMock.ExpectationsWereMet(); err != nil {
		t.Errorf("identity leg: %v", err)
	}
}

// A mirror failure must not turn a completed identity write into a reported
// failure: the caller would retry a grant that already applied.
func TestMirrorFailureDoesNotFailTheOperation(t *testing.T) {
	m, identityMock, appMock, done := twoConnections(t)
	defer done()

	expectIdentityRoleLookup(identityMock, "editor")
	identityMock.ExpectExec("INSERT INTO organization_members").
		WillReturnResult(sqlmock.NewResult(0, 1))
	appMock.ExpectQuery(regexp.QuoteMeta(`SELECT id FROM role_templates WHERE name = $1`)).
		WithArgs("editor").
		WillReturnError(errors.New("mirror is down"))

	if err := m.AddMemberWithParams(context.Background(), "org-1", "user-1", "editor", idstore.OrgScopeOrganizations("org-1")); err != nil {
		t.Fatalf("a mirror failure was surfaced as an operation failure: %v", err)
	}
}

// Guards the promoted-reads property the whole wrapper rests on: everything the
// shared repository exposes and this package does not override must still be
// callable on *Members, or the wrapper would have been a rewrite rather than a
// wrap.
func TestReadsArePromotedUnchanged(t *testing.T) {
	identityDB, identityMock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = identityDB.Close() }()

	identityMock.ExpectQuery("SELECT .* FROM organization_members").
		WillReturnRows(sqlmock.NewRows([]string{"organization_id", "user_id", "role_template_id", "created_at"}).
			AddRow("org-1", "user-1", sql.NullString{}, time.Now()))

	m := NewMembers(identityDB, nil)
	if _, _, err := m.CheckMembership(context.Background(), "org-1", "user-1", idstore.OrgScopeOrganizations("org-1")); err != nil {
		t.Fatalf("a promoted read stopped working: %v", err)
	}
}

// The by-ID update is the admin route's shape (PUT .../members/{user_id} carries
// a role_template_id, not a name). It had no unit coverage at all until this
// test, which is exactly the kind of gap the class guard cannot see: the guard
// proves the method mirrors, not that the mirror it writes is the right row.
func TestUpdateMemberRoleTemplate_WritesBothSides(t *testing.T) {
	m, identityMock, appMock, done := twoConnections(t)
	defer done()

	roleID := templateID
	var tp tape
	identityMock.ExpectExec("UPDATE organization_members").
		WithArgs(tp.at("identity"), "user-1", roleID, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	appMock.ExpectQuery(regexp.QuoteMeta(`SELECT 1 FROM role_templates WHERE id = $1`)).
		WithArgs(roleID).
		WillReturnRows(sqlmock.NewRows([]string{"?column?"}).AddRow(1))
	appMock.ExpectExec("INSERT INTO organization_member_roles").
		WithArgs(tp.at("mirror"), "user-1", roleID).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := m.UpdateMemberRoleTemplate(context.Background(), "org-1", "user-1", &roleID, idstore.OrgScopeOrganizations("org-1")); err != nil {
		t.Fatalf("UpdateMemberRoleTemplate: %v", err)
	}
	tp.wants(t, "identity", "mirror")
	if err := appMock.ExpectationsWereMet(); err != nil {
		t.Errorf("mirror leg: %v", err)
	}
}

// A template id the mirror has never seen must be COPIED from identity before
// the assignment is written. Without this the foreign key rejects the insert and
// the mirror silently loses a grant that did happen — the shared-identity case
// where the sibling app created the role.
func TestMirrorSetByID_AdoptsATemplateItHasNotSeen(t *testing.T) {
	m, identityMock, appMock, done := twoConnections(t)
	defer done()

	roleID := templateID
	identityMock.ExpectExec("INSERT INTO organization_members").
		WillReturnResult(sqlmock.NewResult(0, 1))
	appMock.ExpectQuery(regexp.QuoteMeta(`SELECT 1 FROM role_templates WHERE id = $1`)).
		WithArgs(roleID).
		WillReturnRows(sqlmock.NewRows([]string{"?column?"}))
	identityMock.ExpectQuery("SELECT id, name, display_name, description, scopes, is_system").
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "display_name", "description", "scopes", "is_system", "created_at", "updated_at"}).
			AddRow(roleID, "late", "Late", nil, []byte(`["state:read"]`), true, time.Now(), time.Now()))
	appMock.ExpectExec("INSERT INTO role_templates").
		WithArgs(roleID, "late", "Late", nil, sqlmock.AnyArg(), true).
		WillReturnResult(sqlmock.NewResult(0, 1))
	appMock.ExpectExec("INSERT INTO organization_member_roles").
		WithArgs("org-1", "user-1", roleID).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := m.AddMemberWithRoleTemplate(context.Background(), "org-1", "user-1", &roleID, idstore.OrgScopeOrganizations("org-1")); err != nil {
		t.Fatalf("AddMemberWithRoleTemplate: %v", err)
	}
	if err := appMock.ExpectationsWereMet(); err != nil {
		t.Errorf("mirror leg: %v", err)
	}
	if err := identityMock.ExpectationsWereMet(); err != nil {
		t.Errorf("identity leg: %v", err)
	}
}

// THE MIRROR MUST NOT REACH FURTHER THAN THE WRITE IT MIRRORS.
//
// A revocation writes the mirror FIRST, before the identity leg's scope
// predicate has refused anything, so without a tenancy check a caller whose
// scope does not admit the organization would have the mirrored row deleted and
// then be told the membership was not found — a cross-tenant write reported as a
// no-op. Nothing reads the mirror in Phase 3a, which is precisely why it would
// go unnoticed until the phase that does.
//
// Found by the suite's tenant-scope signature (#719) on the first CI run of this
// change, not by review.
//
// ASSERTED ON A TAPE THAT MUST STAY EMPTY, not on ExpectationsWereMet.
// sqlmock's ExpectationsWereMet reports expectations that were never MET; it
// says nothing about statements that were issued and not expected. Those fail
// the individual call, and the mirror logs a failed leg and carries on — so the
// obvious "stage nothing on the app mock and assert its expectations were met"
// version of this test PASSES WITH THE FIX REVERTED. It did, on all three
// mutations, before this rewrite. Here the app mock stages exactly the statement
// the unguarded code would issue, carrying a tape matcher: if that statement is
// ever issued the tape records it, and an empty tape is the pass.
func TestMirrorNeverTouchesAnOrganizationOutsideTheCallersScope(t *testing.T) {
	cases := []struct {
		name string
		// stageIdentity scripts the identity leg, which is not under test.
		stageIdentity func(sqlmock.Sqlmock)
		// stageMirror stages the statement the UNGUARDED mirror would issue.
		stageMirror func(sqlmock.Sqlmock, *tape)
		run         func(*Members) error
	}{
		{
			name: "remove: scope names another tenant",
			stageIdentity: func(m sqlmock.Sqlmock) {
				m.ExpectExec("DELETE FROM organization_members").WillReturnResult(sqlmock.NewResult(0, 0))
			},
			stageMirror: func(m sqlmock.Sqlmock, tp *tape) {
				m.ExpectExec(regexp.QuoteMeta(`DELETE FROM organization_member_roles WHERE organization_id = $1 AND user_id = $2`)).
					WithArgs(tp.at("mirror"), "user-1").
					WillReturnResult(sqlmock.NewResult(0, 1))
			},
			run: func(m *Members) error {
				return m.RemoveMember(context.Background(), "org-1", "user-1", idstore.OrgScopeOrganizations("org-2"))
			},
		},
		{
			name:          "remove: deny-everything scope",
			stageIdentity: func(m sqlmock.Sqlmock) {},
			stageMirror: func(m sqlmock.Sqlmock, tp *tape) {
				m.ExpectExec(regexp.QuoteMeta(`DELETE FROM organization_member_roles WHERE organization_id = $1 AND user_id = $2`)).
					WithArgs(tp.at("mirror"), "user-1").
					WillReturnResult(sqlmock.NewResult(0, 1))
			},
			run: func(m *Members) error {
				return m.RemoveMember(context.Background(), "org-1", "user-1", idstore.OrgScope{})
			},
		},
		{
			name: "organization delete: scope names another tenant",
			stageIdentity: func(m sqlmock.Sqlmock) {
				m.ExpectExec("DELETE FROM organizations").WillReturnResult(sqlmock.NewResult(0, 0))
			},
			stageMirror: func(m sqlmock.Sqlmock, tp *tape) {
				m.ExpectExec(regexp.QuoteMeta(`DELETE FROM organization_member_roles WHERE organization_id = $1`)).
					WithArgs(tp.at("mirror")).
					WillReturnResult(sqlmock.NewResult(0, 3))
			},
			run: func(m *Members) error {
				return m.Delete(context.Background(), "org-1", idstore.OrgScopeOrganizations("org-2"))
			},
		},
		{
			name: "grant: scope names another tenant",
			stageIdentity: func(m sqlmock.Sqlmock) {
				expectIdentityRoleLookup(m, "editor")
				// Scripted to SUCCEED even though the scope excludes the
				// organization: that is the only way to reach the mirror leg at
				// all, and therefore the only way this assertion can fail.
				m.ExpectExec("INSERT INTO organization_members").WillReturnResult(sqlmock.NewResult(0, 1))
			},
			stageMirror: func(m sqlmock.Sqlmock, tp *tape) {
				m.ExpectQuery(regexp.QuoteMeta(`SELECT id FROM role_templates WHERE name = $1`)).
					WithArgs(tp.at("mirror")).
					WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(templateID))
			},
			run: func(m *Members) error {
				return m.AddMemberWithParams(context.Background(), "org-1", "user-1", "editor", idstore.OrgScopeOrganizations("org-2"))
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m, identityMock, appMock, done := twoConnections(t)
			defer done()
			var tp tape
			c.stageIdentity(identityMock)
			c.stageMirror(appMock, &tp)

			// The identity leg's own answer is not under test — it already
			// refuses out-of-scope writes — so its error is ignored.
			_ = c.run(m)

			if len(tp.steps) != 0 {
				t.Fatalf("the mirror wrote an organization the caller's scope does not permit: %v", tp.steps)
			}
		})
	}
}
