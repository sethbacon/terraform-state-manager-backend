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

// sweeps records the principals a mutation's mandatory credential sweep ran for.
// A recorder rather than a no-op: the parameter exists because an authority
// reduction that does not sweep leaves the credentials which froze that
// authority working, so "it was supplied" is not the property — "it ran, for
// this principal" is.
type sweeps struct {
	users []string
	// changed records, per sweep, whether the caller reported that authority
	// ACTUALLY moved. Recorded rather than ignored because a sweep that runs
	// with the wrong answer here ends every session the principal holds for a
	// write that changed nothing (#491).
	changed []bool
}

func (sw *sweeps) reducer() AuthorityReducer {
	return func(_ context.Context, userID string, authorityChanged bool) error {
		sw.users = append(sw.users, userID)
		sw.changed = append(sw.changed, authorityChanged)
		return nil
	}
}

// wantsChanged asserts the authority-changed flags the sweeps were told, in
// order. Separate from wants so an existing test keeps asserting exactly what
// it always did.
func (sw *sweeps) wantsChanged(t *testing.T, want ...bool) {
	t.Helper()
	if len(sw.changed) != len(want) {
		t.Fatalf("authority-changed flags %v, want %v", sw.changed, want)
	}
	for i := range want {
		if sw.changed[i] != want[i] {
			t.Errorf("sweep %d reported authorityChanged=%v, want %v", i, sw.changed[i], want[i])
		}
	}
}

func (sw *sweeps) wants(t *testing.T, want ...string) {
	t.Helper()
	if len(sw.users) != len(want) {
		t.Fatalf("credential sweep ran for %v, want %v", sw.users, want)
	}
	for i := range want {
		if sw.users[i] != want[i] {
			t.Fatalf("credential sweep ran for %v, want %v", sw.users, want)
		}
	}
}

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
	identityDB, identityMock, err := newSQLMockRegexp()
	if err != nil {
		t.Fatalf("sqlmock.New (identity): %v", err)
	}
	appDB, appMock, err := newSQLMockRegexp()
	if err != nil {
		t.Fatalf("sqlmock.New (app): %v", err)
	}
	m := NewMembers(identityDB, appDB, RoleSourceIdentity)
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
	var sw sweeps

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
		WithArgs(tp.at("mirror"), "user-1", templateID, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := m.AddMemberWithParams(context.Background(), "org-1", "user-1", "editor", idstore.OrgScopeOrganizations("org-1"), sw.reducer()); err != nil {
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
	var sw sweeps

	expectIdentityRoleLookup(identityMock, "editor")
	identityMock.ExpectExec("INSERT INTO organization_members").
		WillReturnError(errors.New("insert failed"))
	// appMock has NO expectations: any statement at all fails the test.

	err := m.AddMemberWithParams(context.Background(), "org-1", "user-1", "editor", idstore.OrgScopeOrganizations("org-1"), sw.reducer())
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
	var sw sweeps

	roleID := templateID
	identityMock.ExpectExec("INSERT INTO organization_members").
		WillReturnResult(sqlmock.NewResult(0, 1))
	// The id carried over from identity is already in TSM's own table, so no
	// template fetch is needed.
	appMock.ExpectQuery(regexp.QuoteMeta(`SELECT 1 FROM role_templates WHERE id = $1`)).
		WithArgs(roleID).
		WillReturnRows(sqlmock.NewRows([]string{"?column?"}).AddRow(1))
	appMock.ExpectExec("INSERT INTO organization_member_roles").
		WithArgs("org-1", "user-1", roleID, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := m.AddMemberWithRoleTemplate(context.Background(), "org-1", "user-1", &roleID, idstore.OrgScopeOrganizations("org-1"), sw.reducer()); err != nil {
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
	var sw sweeps

	identityMock.ExpectExec("INSERT INTO organization_members").
		WillReturnResult(sqlmock.NewResult(0, 1))
	appMock.ExpectExec("INSERT INTO organization_member_roles").
		WithArgs("org-1", "user-1", nil, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := m.AddMemberWithRoleTemplate(context.Background(), "org-1", "user-1", nil, idstore.OrgScopeOrganizations("org-1"), sw.reducer()); err != nil {
		t.Fatalf("AddMemberWithRoleTemplate: %v", err)
	}
	if err := appMock.ExpectationsWereMet(); err != nil {
		t.Errorf("mirror leg: %v", err)
	}
}

func TestUpdateMemberRole_WritesBothSides(t *testing.T) {
	m, identityMock, appMock, done := twoConnections(t)
	defer done()
	var sw sweeps

	expectIdentityRoleLookup(identityMock, "viewer")
	identityMock.ExpectExec("UPDATE organization_members").
		WillReturnResult(sqlmock.NewResult(0, 1))
	appMock.ExpectQuery(regexp.QuoteMeta(`SELECT id FROM role_templates WHERE name = $1`)).
		WithArgs("viewer").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(templateID))
	appMock.ExpectExec("INSERT INTO organization_member_roles").
		WithArgs("org-1", "user-1", templateID, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := m.UpdateMemberRole(context.Background(), "org-1", "user-1", "viewer", idstore.OrgScopeOrganizations("org-1"), sw.reducer()); err != nil {
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
	var sw sweeps

	var tp tape
	appMock.ExpectExec(regexp.QuoteMeta(`DELETE FROM organization_member_roles WHERE organization_id = $1 AND user_id = $2 AND organization_id = ANY($3)`)).
		WithArgs(tp.at("mirror"), "user-1", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	identityMock.ExpectExec("DELETE FROM organization_members").
		WithArgs(tp.at("identity"), "user-1", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := m.RemoveMember(context.Background(), "org-1", "user-1", idstore.OrgScopeOrganizations("org-1"), sw.reducer()); err != nil {
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
	var sw sweeps

	appMock.ExpectExec("DELETE FROM organization_member_roles").
		WithArgs("org-1", "user-1", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 0))
	identityMock.ExpectExec("DELETE FROM organization_members").
		WillReturnResult(sqlmock.NewResult(0, 0))

	err := m.RemoveMember(context.Background(), "org-1", "user-1", idstore.OrgScopeOrganizations("org-1"), sw.reducer())
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
	var sw sweeps

	var tp tape
	// Snapshot first: the cascade removes the members, so the sweep's subjects
	// have to be read before the delete rather than after it.
	identityMock.ExpectQuery("FROM organization_members").
		WillReturnRows(sqlmock.NewRows([]string{"organization_id", "user_id", "role_template_id", "created_at"}).
			AddRow("org-1", "user-9", sql.NullString{}, time.Now()))
	appMock.ExpectExec(regexp.QuoteMeta(`DELETE FROM organization_member_roles WHERE organization_id = $1 AND organization_id = ANY($2)`)).
		WithArgs(tp.at("mirror"), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 3))
	identityMock.ExpectExec("DELETE FROM organizations").
		WithArgs(tp.at("identity"), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := m.Delete(context.Background(), "org-1", idstore.OrgScopeOrganizations("org-1"), sw.reducer()); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	tp.wants(t, "mirror", "identity")
	// Deleting an organization withdraws every member's authority with no
	// membership statement of its own; the sweep has to reach each of them.
	sw.wants(t, "user-9")
	if err := appMock.ExpectationsWereMet(); err != nil {
		t.Errorf("mirror leg: %v", err)
	}
}

// The platform-wide strip must reach EVERY organization in the mirror, and the
// post-pass must narrow to what identity says it actually emptied.
func TestRemoveAllMembershipsForUser_StripsBeforeAndAfter(t *testing.T) {
	m, identityMock, appMock, done := twoConnections(t)
	defer done()
	var sw sweeps

	// Pre-pass: the scope is platform-wide, so OrgScope.SQL emits the literal
	// TRUE — never an empty clause, which is what makes an undecided caller's
	// zero value a literal FALSE rather than an unfiltered statement.
	appMock.ExpectExec(regexp.QuoteMeta(`DELETE FROM organization_member_roles WHERE user_id = $1 AND TRUE`)).
		WithArgs("user-1").
		WillReturnResult(sqlmock.NewResult(0, 2))
	identityMock.ExpectQuery("DELETE FROM organization_members").
		WillReturnRows(sqlmock.NewRows([]string{"organization_id"}).AddRow("org-1").AddRow("org-2"))
	// Post-pass: narrowed to exactly the organizations identity reports it
	// emptied, which is an allowlist and therefore a bound ANY().
	appMock.ExpectExec(regexp.QuoteMeta(`DELETE FROM organization_member_roles WHERE user_id = $1 AND organization_id = ANY($2)`)).
		WithArgs("user-1", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 0))

	removed, err := m.RemoveAllMembershipsForUser(context.Background(), "user-1", idstore.OrgScopeAllOrganizations(), sw.reducer())
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

// PurgeUserRoles is the pairing the class guard requires at every DeleteUser
// call site, so it must actually strip every organization.
func TestPurgeUserRoles_StripsEveryOrganization(t *testing.T) {
	m, _, appMock, done := twoConnections(t)
	defer done()

	appMock.ExpectExec(regexp.QuoteMeta(`DELETE FROM organization_member_roles WHERE user_id = $1 AND TRUE`)).
		WithArgs("user-1").
		WillReturnResult(sqlmock.NewResult(0, 2))

	m.PurgeUserRoles(context.Background(), "user-1", idstore.OrgScopeAllOrganizations())
	if err := appMock.ExpectationsWereMet(); err != nil {
		t.Errorf("mirror leg: %v", err)
	}
}

// Constructed without an app connection, the mirror is absent and every write
// degrades to the identity leg alone rather than panicking. That is the unit-test
// rig's shape, and it must not be the server's.
func TestNoAppConnection_DegradesToTheIdentityLegAlone(t *testing.T) {
	identityDB, identityMock, err := newSQLMockRegexp()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = identityDB.Close() }()
	var sw sweeps

	m := NewMembers(identityDB, nil, RoleSourceIdentity)
	if m.Store() != nil {
		t.Fatal("a Members built without an app connection reported a mirror store")
	}
	identityMock.ExpectExec("DELETE FROM organization_members").
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := m.RemoveMember(context.Background(), "org-1", "user-1", idstore.OrgScopeOrganizations("org-1"), sw.reducer()); err != nil {
		t.Fatalf("RemoveMember: %v", err)
	}
	if err := identityMock.ExpectationsWereMet(); err != nil {
		t.Errorf("identity leg: %v", err)
	}
}

// A MIRROR FAILURE FAILS THE OPERATION. This assertion is the INVERSE of the one
// it replaces, and the inversion is Phase 3b.
//
// While identity was the authority, a failed mirror leg was a row nobody read, so
// reporting it would have told a caller their grant failed when it had not. Now
// these tables ARE the authority: swallowing the failure returns 200 for a role
// change this application did not make, and the principal keeps whatever the
// mirror still says. A demotion is the case that matters — 200, an audit entry
// recording it, and the administrator scopes still granted.
func TestAMirrorFailureFailsTheOperation(t *testing.T) {
	m, identityMock, appMock, done := twoConnections(t)
	defer done()
	var sw sweeps

	expectIdentityRoleLookup(identityMock, "editor")
	identityMock.ExpectExec("INSERT INTO organization_members").
		WillReturnResult(sqlmock.NewResult(0, 1))
	appMock.ExpectQuery(regexp.QuoteMeta(`SELECT id FROM role_templates WHERE name = $1`)).
		WithArgs("editor").
		WillReturnError(errors.New("mirror is down"))

	err := m.AddMemberWithParams(context.Background(), "org-1", "user-1", "editor", idstore.OrgScopeOrganizations("org-1"), sw.reducer())
	if err == nil {
		t.Fatal("a mirror failure was reported as success: the caller believes a role change applied that " +
			"this application's authorization tables do not record")
	}
	if !strings.Contains(err.Error(), "mirror is down") {
		t.Fatalf("the underlying failure was not reported: %v", err)
	}
}

// THE FAILURE ON THE ROW ITSELF, which is the one that matters and the one the
// test above does NOT reach: it stages its fault at the role-NAME lookup, so a
// version that propagated the lookup error and swallowed the SetRole error passed
// it. Mutation found that. The write to organization_member_roles is what decides
// what this principal may do, so its failure is the failure this test exists for.
//
// Table-driven over both legs of mirrorSetByName so neither can be covered while
// the other is not.
func TestAMirrorWriteFailureFailsTheOperation(t *testing.T) {
	cases := []struct {
		name  string
		stage func(appMock sqlmock.Sqlmock)
	}{
		{
			name: "the role name cannot be resolved",
			stage: func(appMock sqlmock.Sqlmock) {
				appMock.ExpectQuery(regexp.QuoteMeta(`SELECT id FROM role_templates WHERE name = $1`)).
					WithArgs("editor").
					WillReturnError(errors.New("mirror is down"))
			},
		},
		{
			name: "the role record cannot be written",
			stage: func(appMock sqlmock.Sqlmock) {
				appMock.ExpectQuery(regexp.QuoteMeta(`SELECT id FROM role_templates WHERE name = $1`)).
					WithArgs("editor").
					WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(templateID))
				appMock.ExpectExec("INSERT INTO organization_member_roles").
					WillReturnError(errors.New("mirror is down"))
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m, identityMock, appMock, done := twoConnections(t)
			defer done()
			var sw sweeps

			expectIdentityRoleLookup(identityMock, "editor")
			identityMock.ExpectExec("INSERT INTO organization_members").
				WillReturnResult(sqlmock.NewResult(0, 1))
			c.stage(appMock)

			err := m.AddMemberWithParams(context.Background(), "org-1", "user-1", "editor", idstore.OrgScopeOrganizations("org-1"), sw.reducer())
			if err == nil {
				t.Fatal("reported success: the caller believes a role change applied that this application's " +
					"authorization tables do not record, and the principal keeps whatever the mirror still says")
			}
			if !strings.Contains(err.Error(), "mirror is down") {
				t.Fatalf("the underlying failure was not reported: %v", err)
			}
		})
	}
}

// A REVOCATION WHOSE MIRROR LEG FAILS MUST NOT TOUCH IDENTITY.
//
// The mirror goes first on a revocation, so a failure there means nothing has
// been withdrawn anywhere and the caller's retry is a retry of an operation that
// did not happen. Proceeding to identity would leave it saying "not a member"
// while this application — the authority — still granted the role: a withdrawal
// that reads as done and is not.
//
// Asserted by the identity mock having NO expectation: any statement on that
// connection fails this test, which is the assertion ExpectationsWereMet cannot
// make.
func TestAFailedRevocationMirrorDoesNotReachIdentity(t *testing.T) {
	m, identityMock, appMock, done := twoConnections(t)
	defer done()

	appMock.ExpectExec("DELETE FROM organization_member_roles").
		WillReturnError(errors.New("mirror is down"))

	var sw sweeps
	err := m.RemoveMember(context.Background(), "org-1", "user-1", idstore.OrgScopeOrganizations("org-1"), sw.reducer())
	if err == nil {
		t.Fatal("a failed mirror revocation was reported as success")
	}
	if !strings.Contains(err.Error(), "mirror is down") {
		t.Fatalf("the underlying failure was not reported: %v", err)
	}
	if err := identityMock.ExpectationsWereMet(); err != nil {
		t.Errorf("identity leg: %v", err)
	}
}

// Guards the promoted-reads property the whole wrapper rests on: everything the
// shared repository exposes and this package does not override must still be
// callable on *Members, or the wrapper would have been a rewrite rather than a
// wrap.
func TestReadsArePromotedUnchanged(t *testing.T) {
	identityDB, identityMock, err := newSQLMockRegexp()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = identityDB.Close() }()

	identityMock.ExpectQuery("SELECT .* FROM organization_members").
		WillReturnRows(sqlmock.NewRows([]string{"organization_id", "user_id", "role_template_id", "created_at"}).
			AddRow("org-1", "user-1", sql.NullString{}, time.Now()))

	m := NewMembers(identityDB, nil, RoleSourceIdentity)
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
	var sw sweeps

	roleID := templateID
	var tp tape
	identityMock.ExpectExec("UPDATE organization_members").
		WithArgs(tp.at("identity"), "user-1", roleID, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	appMock.ExpectQuery(regexp.QuoteMeta(`SELECT 1 FROM role_templates WHERE id = $1`)).
		WithArgs(roleID).
		WillReturnRows(sqlmock.NewRows([]string{"?column?"}).AddRow(1))
	appMock.ExpectExec("INSERT INTO organization_member_roles").
		WithArgs(tp.at("mirror"), "user-1", roleID, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := m.UpdateMemberRoleTemplate(context.Background(), "org-1", "user-1", &roleID, idstore.OrgScopeOrganizations("org-1"), sw.reducer()); err != nil {
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
	var sw sweeps

	roleID := templateID
	identityMock.ExpectExec("INSERT INTO organization_members").
		WillReturnResult(sqlmock.NewResult(0, 1))
	appMock.ExpectQuery(regexp.QuoteMeta(`SELECT 1 FROM role_templates WHERE id = $1`)).
		WithArgs(roleID).
		WillReturnRows(sqlmock.NewRows([]string{"?column?"}))
	identityMock.ExpectQuery("SELECT id, name, display_name, description, scopes, is_system").
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "display_name", "description", "scopes", "is_system", "created_at", "updated_at"}).
			AddRow(roleID, "late", "Late", nil, []byte(`["state:read"]`), true, time.Now(), time.Now()))
	// THE NAME IS LOOKED UP, NOT RELEASED. An earlier version of this deleted the
	// row holding the name under another id — which, through ON DELETE SET NULL,
	// silently withdrew that role from every OTHER principal holding it, on a
	// request that was granting a role to one (security-orchestration#732). The
	// resolution is a read: if the name is already defined here, its LOCAL id is
	// what this application records.
	appMock.ExpectQuery(regexp.QuoteMeta(`SELECT id FROM role_templates WHERE name = $1`)).
		WithArgs("late").
		WillReturnError(sql.ErrNoRows)
	appMock.ExpectExec("INSERT INTO role_templates").
		WithArgs(roleID, "late", "Late", nil, sqlmock.AnyArg(), true).
		WillReturnResult(sqlmock.NewResult(0, 1))
	appMock.ExpectExec("INSERT INTO organization_member_roles").
		WithArgs("org-1", "user-1", roleID, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := m.AddMemberWithRoleTemplate(context.Background(), "org-1", "user-1", &roleID, idstore.OrgScopeOrganizations("org-1"), sw.reducer()); err != nil {
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
// The suite's tenant-scope signature (#719) found this class in this package on
// this branch's first CI run: a revocation mirrors BEFORE the identity leg's
// predicate has refused anything, so an out-of-tenancy caller had another
// tenant's mirrored row deleted and was then told the membership was not found.
//
// WHAT THIS ASSERTS, AND WHY IT CHANGED SHAPE. The first fix branched on the
// scope in this layer, so the property was "no statement is issued" and the test
// was a matcher that must never fire. The predicate now lives in the SQL
// (Store.andScope), so the statement IS issued and matches no row — which is the
// better design and a different observable. What must hold here is that the
// CALLER'S scope is what reaches the statement: these expectations bind
// `organization_id = ANY($n)`, so passing OrgScopeAllOrganizations() instead of
// the caller's scope emits the literal TRUE, fails to match, and fails the test.
//
// The ground truth for "another tenant's row survives" is
// TestIntegrationMirrorIsTenantScoped, which asserts on rows in a real database
// rather than on statement shape.
func TestTheCallersScopeReachesTheMirroredStatement(t *testing.T) {
	t.Run("revocation", func(t *testing.T) {
		m, identityMock, appMock, done := twoConnections(t)
		defer done()
		var sw sweeps

		appMock.ExpectExec(regexp.QuoteMeta(
			`DELETE FROM organization_member_roles WHERE organization_id = $1 AND user_id = $2 AND organization_id = ANY($3)`)).
			WithArgs("org-1", "user-1", sqlmock.AnyArg()).
			WillReturnResult(sqlmock.NewResult(0, 0))
		identityMock.ExpectExec("DELETE FROM organization_members").
			WillReturnResult(sqlmock.NewResult(0, 0))

		_ = m.RemoveMember(context.Background(), "org-1", "user-1", idstore.OrgScopeOrganizations("org-2"), sw.reducer())
		if err := appMock.ExpectationsWereMet(); err != nil {
			t.Fatalf("the caller's scope did not reach the mirrored statement: %v", err)
		}
	})

	t.Run("organization delete", func(t *testing.T) {
		m, identityMock, appMock, done := twoConnections(t)
		defer done()
		var sw sweeps

		// Delete snapshots the members it is about to strip, so the sweep has
		// somebody to run for after the cascade.
		identityMock.ExpectQuery("FROM organization_members").
			WillReturnRows(sqlmock.NewRows([]string{"organization_id", "user_id", "role_template_id", "created_at"}))
		appMock.ExpectExec(regexp.QuoteMeta(
			`DELETE FROM organization_member_roles WHERE organization_id = $1 AND organization_id = ANY($2)`)).
			WithArgs("org-1", sqlmock.AnyArg()).
			WillReturnResult(sqlmock.NewResult(0, 0))
		identityMock.ExpectExec("DELETE FROM organizations").
			WillReturnResult(sqlmock.NewResult(0, 0))

		_ = m.Delete(context.Background(), "org-1", idstore.OrgScopeOrganizations("org-2"), sw.reducer())
		if err := appMock.ExpectationsWereMet(); err != nil {
			t.Fatalf("the caller's scope did not reach the mirrored statement: %v", err)
		}
	})

	t.Run("grant", func(t *testing.T) {
		m, identityMock, appMock, done := twoConnections(t)
		defer done()
		var sw sweeps

		expectIdentityRoleLookup(identityMock, "editor")
		identityMock.ExpectExec("INSERT INTO organization_members").
			WillReturnResult(sqlmock.NewResult(0, 1))
		appMock.ExpectQuery(regexp.QuoteMeta(`SELECT id FROM role_templates WHERE name = $1`)).
			WithArgs("editor").
			WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(templateID))
		appMock.ExpectExec(regexp.QuoteMeta(`WHERE TRUE AND v.organization_id = ANY($4)`)).
			WithArgs("org-1", "user-1", templateID, sqlmock.AnyArg()).
			WillReturnResult(sqlmock.NewResult(0, 0))

		_ = m.AddMemberWithParams(context.Background(), "org-1", "user-1", "editor", idstore.OrgScopeOrganizations("org-2"), sw.reducer())
		if err := appMock.ExpectationsWereMet(); err != nil {
			t.Fatalf("the caller's scope did not reach the mirrored statement: %v", err)
		}
	})
}

// A MISSING SWEEP FAILS CLOSED. The reducer is mandatory at the type level — the
// four-argument RemoveMember a caller used to write no longer compiles — but a
// caller can still hand over a nil of the right type, and "optional" is how the
// carrier's floor predicate went silently absent in the library's own
// extraction. An authority reduction that runs with no sweep leaves the
// credentials which froze that authority working, which is #330 exactly.
func TestAMissingSweepIsRefused(t *testing.T) {
	m, identityMock, appMock, done := twoConnections(t)
	defer done()

	appMock.ExpectExec("DELETE FROM organization_member_roles").
		WithArgs("org-1", "user-1", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	identityMock.ExpectExec("DELETE FROM organization_members").
		WillReturnResult(sqlmock.NewResult(0, 1))

	err := m.RemoveMember(context.Background(), "org-1", "user-1", idstore.OrgScopeOrganizations("org-1"), nil)
	if err == nil {
		t.Fatal("an authority reduction ran with no credential sweep and reported success")
	}
	if !strings.Contains(err.Error(), "no credential sweep") {
		t.Fatalf("the error does not say what was missing: %v", err)
	}
}

// The sweep must run for the principal whose authority was reduced, on every
// reducing path. Table-driven over the whole class rather than one method: the
// defect this guards is per-path, and a test that covered only RemoveMember
// would pass forever while a sibling silently stopped sweeping.
func TestEveryReducingPathSweepsItsSubject(t *testing.T) {
	roleID := templateID
	cases := []struct {
		name  string
		stage func(identity, app sqlmock.Sqlmock)
		run   func(*Members, AuthorityReducer) error
	}{
		{
			name: "membership removed",
			stage: func(identity, app sqlmock.Sqlmock) {
				app.ExpectExec("DELETE FROM organization_member_roles").WillReturnResult(sqlmock.NewResult(0, 1))
				identity.ExpectExec("DELETE FROM organization_members").WillReturnResult(sqlmock.NewResult(0, 1))
			},
			run: func(m *Members, r AuthorityReducer) error {
				return m.RemoveMember(context.Background(), "org-1", "user-1", idstore.OrgScopeOrganizations("org-1"), r)
			},
		},
		{
			name: "role reassigned by id",
			stage: func(identity, app sqlmock.Sqlmock) {
				identity.ExpectExec("UPDATE organization_members").WillReturnResult(sqlmock.NewResult(0, 1))
				app.ExpectQuery(regexp.QuoteMeta(`SELECT 1 FROM role_templates WHERE id = $1`)).
					WillReturnRows(sqlmock.NewRows([]string{"?column?"}).AddRow(1))
				app.ExpectExec("INSERT INTO organization_member_roles").WillReturnResult(sqlmock.NewResult(0, 1))
			},
			run: func(m *Members, r AuthorityReducer) error {
				return m.UpdateMemberRoleTemplate(context.Background(), "org-1", "user-1", &roleID, idstore.OrgScopeOrganizations("org-1"), r)
			},
		},
		{
			name: "role reassigned by name",
			stage: func(identity, app sqlmock.Sqlmock) {
				expectIdentityRoleLookup(identity, "viewer")
				identity.ExpectExec("UPDATE organization_members").WillReturnResult(sqlmock.NewResult(0, 1))
				app.ExpectQuery(regexp.QuoteMeta(`SELECT id FROM role_templates WHERE name = $1`)).
					WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(templateID))
				app.ExpectExec("INSERT INTO organization_member_roles").WillReturnResult(sqlmock.NewResult(0, 1))
			},
			run: func(m *Members, r AuthorityReducer) error {
				return m.UpdateMemberRole(context.Background(), "org-1", "user-1", "viewer", idstore.OrgScopeOrganizations("org-1"), r)
			},
		},
		{
			name: "every membership stripped",
			stage: func(identity, app sqlmock.Sqlmock) {
				app.ExpectExec("DELETE FROM organization_member_roles").WillReturnResult(sqlmock.NewResult(0, 1))
				identity.ExpectQuery("DELETE FROM organization_members").
					WillReturnRows(sqlmock.NewRows([]string{"organization_id"}).AddRow("org-1"))
				app.ExpectExec("DELETE FROM organization_member_roles").WillReturnResult(sqlmock.NewResult(0, 1))
			},
			run: func(m *Members, r AuthorityReducer) error {
				_, err := m.RemoveAllMembershipsForUser(context.Background(), "user-1", idstore.OrgScopeAllOrganizations(), r)
				return err
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m, identityMock, appMock, done := twoConnections(t)
			defer done()
			var sw sweeps
			c.stage(identityMock, appMock)
			if err := c.run(m, sw.reducer()); err != nil {
				t.Fatalf("%s: %v", c.name, err)
			}
			sw.wants(t, "user-1")
		})
	}
}

// A ROLE NAME THIS APPLICATION ALREADY DEFINES RESOLVES TO ITS OWN ID, and no
// row is deleted to make room for identity's.
//
// This is the case Phase 3b created and security-orchestration#732 caught. The
// app-side seed mints a LOCAL uuid for a role name identity does not have, so
// `operator` can exist here under uuid Y while the sibling later seeds
// `operator` into identity under uuid Z. Releasing the name — which is what the
// startup reconcile does, where the assignment pass restates everything
// microseconds later — would SET NULL every assignment referencing Y: a silent
// mass authority reduction, with no credential sweep, on a request that was
// granting a role to ONE principal.
//
// Asserted on the id that reaches organization_member_roles, and on the DELETE
// never being issued: the app mock has no expectation for one.
func TestMirrorSetByID_UsesTheLocalIDForAnAlreadyDefinedName(t *testing.T) {
	m, identityMock, appMock, done := twoConnections(t)
	defer done()
	var sw sweeps

	const identityID = "eeeeeeee-0000-0000-0000-000000000001"
	const localID = "ffffffff-0000-0000-0000-000000000002"
	roleID := identityID

	identityMock.ExpectExec("INSERT INTO organization_members").
		WillReturnResult(sqlmock.NewResult(0, 1))
	// Identity's id is unknown here...
	appMock.ExpectQuery(regexp.QuoteMeta(`SELECT 1 FROM role_templates WHERE id = $1`)).
		WithArgs(identityID).
		WillReturnRows(sqlmock.NewRows([]string{"?column?"}))
	identityMock.ExpectQuery("SELECT id, name, display_name, description, scopes, is_system").
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "display_name", "description", "scopes", "is_system", "created_at", "updated_at"}).
			AddRow(identityID, "operator", "Operator", nil, []byte(`["state:read"]`), true, time.Now(), time.Now()))
	// ...but its NAME is, under a locally-minted id.
	appMock.ExpectQuery(regexp.QuoteMeta(`SELECT id FROM role_templates WHERE name = $1`)).
		WithArgs("operator").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(localID))
	// THE LOCAL ID IS WHAT IS RECORDED. Any DELETE or INSERT on role_templates
	// here is an unexpected statement and fails this test.
	appMock.ExpectExec("INSERT INTO organization_member_roles").
		WithArgs("org-1", "user-1", localID, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := m.AddMemberWithRoleTemplate(context.Background(), "org-1", "user-1", &roleID,
		idstore.OrgScopeOrganizations("org-1"), sw.reducer()); err != nil {
		t.Fatalf("AddMemberWithRoleTemplate: %v", err)
	}
	if err := appMock.ExpectationsWereMet(); err != nil {
		t.Errorf("mirror leg: %v", err)
	}
}

// THE ADD PATHS SWEEP. Their mirror leg is an upsert on the table that now
// decides authorization, so adding a principal to an organization identity has
// no membership for can move a STALE record downward — a reduction performed by
// a method called Add (security-orchestration#732).
//
// Asserted on the sweep having RUN for the right principal, not on the call
// succeeding: a reducer that is accepted and never invoked satisfies the
// signature and closes nothing.
func TestTheAddPathsRunTheCallersSweep(t *testing.T) {
	for _, c := range []struct {
		name string
		call func(m *Members, reduce AuthorityReducer) error
	}{
		{"by name", func(m *Members, reduce AuthorityReducer) error {
			return m.AddMemberWithParams(context.Background(), "org-1", "user-1", "editor",
				idstore.OrgScopeOrganizations("org-1"), reduce)
		}},
		{"by id", func(m *Members, reduce AuthorityReducer) error {
			roleID := templateID
			return m.AddMemberWithRoleTemplate(context.Background(), "org-1", "user-1", &roleID,
				idstore.OrgScopeOrganizations("org-1"), reduce)
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			m, identityMock, appMock, done := twoConnections(t)
			defer done()
			var sw sweeps

			if c.name == "by name" {
				expectIdentityRoleLookup(identityMock, "editor")
			}
			identityMock.ExpectExec("INSERT INTO organization_members").
				WillReturnResult(sqlmock.NewResult(0, 1))
			if c.name == "by name" {
				appMock.ExpectQuery(regexp.QuoteMeta(`SELECT id FROM role_templates WHERE name = $1`)).
					WithArgs("editor").
					WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(templateID))
			} else {
				appMock.ExpectQuery(regexp.QuoteMeta(`SELECT 1 FROM role_templates WHERE id = $1`)).
					WithArgs(templateID).
					WillReturnRows(sqlmock.NewRows([]string{"?column?"}).AddRow(1))
			}
			appMock.ExpectExec("INSERT INTO organization_member_roles").
				WillReturnResult(sqlmock.NewResult(0, 1))

			if err := c.call(m, sw.reducer()); err != nil {
				t.Fatalf("add: %v", err)
			}
			sw.wants(t, "user-1")
		})
	}
}

// A nil reducer is refused on the add paths too, so a caller cannot reach the
// upsert without deciding.
func TestTheAddPathsRefuseAMissingSweep(t *testing.T) {
	m, identityMock, appMock, done := twoConnections(t)
	defer done()

	expectIdentityRoleLookup(identityMock, "editor")
	identityMock.ExpectExec("INSERT INTO organization_members").
		WillReturnResult(sqlmock.NewResult(0, 1))
	appMock.ExpectQuery(regexp.QuoteMeta(`SELECT id FROM role_templates WHERE name = $1`)).
		WithArgs("editor").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(templateID))
	appMock.ExpectExec("INSERT INTO organization_member_roles").
		WillReturnResult(sqlmock.NewResult(0, 1))

	err := m.AddMemberWithParams(context.Background(), "org-1", "user-1", "editor",
		idstore.OrgScopeOrganizations("org-1"), nil)
	if err == nil {
		t.Fatal("an add ran with no credential sweep supplied")
	}
	if !strings.Contains(err.Error(), "no credential sweep") {
		t.Fatalf("the error does not say what is missing: %v", err)
	}
}

// ---------------------------------------------------------------------------
// #491 — a write that changed nothing must not end the principal's sessions.
//
// The API-key half of a sweep re-derives the retained authority and keeps every
// key still covered, so it is harmless on a no-op. The token half moves a
// PLATFORM-WIDE per-user watermark and ends every session that principal holds,
// in every organization. Running that for a write that moved nothing is pure
// damage, and both no-op paths are reachable through routes that deliberately
// absorb "not a member" into a success status.
// ---------------------------------------------------------------------------

// expectPriorRole stages the pre-write read of the role currently recorded.
func expectPriorRole(mock sqlmock.Sqlmock, roleID interface{}) {
	mock.ExpectQuery("(?s)SELECT organization_id, user_id, role_template_id, created_at.*FROM organization_members").
		WillReturnRows(sqlmock.NewRows([]string{"organization_id", "user_id", "role_template_id", "created_at"}).
			AddRow("org-1", "user-1", roleID, time.Now()))
}

func TestUpdateMemberRoleTemplate_SameRoleDoesNotEndSessions(t *testing.T) {
	m, identityMock, appMock, done := twoConnections(t)
	defer done()
	var sw sweeps

	roleID := templateID
	expectPriorRole(identityMock, roleID) // already holds it
	identityMock.ExpectExec("UPDATE organization_members").
		WillReturnResult(sqlmock.NewResult(0, 1))
	appMock.ExpectQuery(regexp.QuoteMeta(`SELECT 1 FROM role_templates WHERE id = $1`)).
		WithArgs(roleID).
		WillReturnRows(sqlmock.NewRows([]string{"?column?"}).AddRow(1))
	appMock.ExpectExec("INSERT INTO organization_member_roles").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := m.UpdateMemberRoleTemplate(context.Background(), "org-1", "user-1", &roleID, idstore.OrgScopeOrganizations("org-1"), sw.reducer()); err != nil {
		t.Fatalf("UpdateMemberRoleTemplate: %v", err)
	}
	sw.wants(t, "user-1")     // the key half still runs
	sw.wantsChanged(t, false) // the session half must not
}

// The falsification: a genuine reassignment must still end the sessions that
// froze the old authority. Without this, reporting false unconditionally would
// satisfy the test above and disable #330 entirely.
func TestUpdateMemberRoleTemplate_DifferentRoleEndsSessions(t *testing.T) {
	m, identityMock, appMock, done := twoConnections(t)
	defer done()
	var sw sweeps

	roleID := templateID
	expectPriorRole(identityMock, "00000000-0000-0000-0000-0000000000ff") // a different role
	identityMock.ExpectExec("UPDATE organization_members").
		WillReturnResult(sqlmock.NewResult(0, 1))
	appMock.ExpectQuery(regexp.QuoteMeta(`SELECT 1 FROM role_templates WHERE id = $1`)).
		WithArgs(roleID).
		WillReturnRows(sqlmock.NewRows([]string{"?column?"}).AddRow(1))
	appMock.ExpectExec("INSERT INTO organization_member_roles").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := m.UpdateMemberRoleTemplate(context.Background(), "org-1", "user-1", &roleID, idstore.OrgScopeOrganizations("org-1"), sw.reducer()); err != nil {
		t.Fatalf("UpdateMemberRoleTemplate: %v", err)
	}
	sw.wantsChanged(t, true)
}

// A prior-role read that FAILS must report changed, not unchanged. Uncertainty
// has to cost an unnecessary sweep, never a missed reduction.
func TestUpdateMemberRoleTemplate_UnreadablePriorRoleEndsSessions(t *testing.T) {
	m, identityMock, appMock, done := twoConnections(t)
	defer done()
	var sw sweeps

	roleID := templateID
	identityMock.ExpectQuery("(?s)SELECT organization_id, user_id, role_template_id, created_at.*FROM organization_members").
		WillReturnError(errors.New("identity database unavailable"))
	identityMock.ExpectExec("UPDATE organization_members").
		WillReturnResult(sqlmock.NewResult(0, 1))
	appMock.ExpectQuery(regexp.QuoteMeta(`SELECT 1 FROM role_templates WHERE id = $1`)).
		WithArgs(roleID).
		WillReturnRows(sqlmock.NewRows([]string{"?column?"}).AddRow(1))
	appMock.ExpectExec("INSERT INTO organization_member_roles").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := m.UpdateMemberRoleTemplate(context.Background(), "org-1", "user-1", &roleID, idstore.OrgScopeOrganizations("org-1"), sw.reducer()); err != nil {
		t.Fatalf("UpdateMemberRoleTemplate: %v", err)
	}
	sw.wantsChanged(t, true)
}

// DELETE naming a user who is not a member: the route absorbs ErrNotFound into
// a 204, which made this a way to sign a stranger out of every organization,
// from an organization they were never in.
func TestRemoveMember_NonMemberDoesNotEndSessions(t *testing.T) {
	m, identityMock, appMock, done := twoConnections(t)
	defer done()
	var sw sweeps

	appMock.ExpectExec("DELETE FROM organization_member_roles").
		WillReturnResult(sqlmock.NewResult(0, 0))
	identityMock.ExpectExec("DELETE FROM organization_members").
		WillReturnResult(sqlmock.NewResult(0, 0)) // no row: ErrNotFound

	err := m.RemoveMember(context.Background(), "org-1", "user-1", idstore.OrgScopeOrganizations("org-1"), sw.reducer())
	if !errors.Is(err, idstore.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
	sw.wants(t, "user-1")     // the key half still runs, as it always did
	sw.wantsChanged(t, false) // the session half must not
}

func TestRemoveMember_ActualRemovalEndsSessions(t *testing.T) {
	m, identityMock, appMock, done := twoConnections(t)
	defer done()
	var sw sweeps

	appMock.ExpectExec("DELETE FROM organization_member_roles").
		WillReturnResult(sqlmock.NewResult(0, 1))
	identityMock.ExpectExec("DELETE FROM organization_members").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := m.RemoveMember(context.Background(), "org-1", "user-1", idstore.OrgScopeOrganizations("org-1"), sw.reducer()); err != nil {
		t.Fatalf("RemoveMember: %v", err)
	}
	sw.wantsChanged(t, true)
}

// CLEARING a member's role is a reduction, and it is the case where the
// unreadable-prior-role guard actually bites.
//
// With the read failing, `before` is nil. Comparing a nil prior against a
// PRESENT new role already yields "changed", so the guard looks redundant --
// until the new role is ALSO nil, when a nil-vs-nil comparison says "unchanged"
// and the sessions holding the old authority survive a write that removed it.
func TestUpdateMemberRoleTemplate_ClearingRoleWithUnreadablePriorEndsSessions(t *testing.T) {
	m, identityMock, appMock, done := twoConnections(t)
	defer done()
	var sw sweeps

	identityMock.ExpectQuery("(?s)SELECT organization_id, user_id, role_template_id, created_at.*FROM organization_members").
		WillReturnError(errors.New("identity database unavailable"))
	identityMock.ExpectExec("UPDATE organization_members").
		WillReturnResult(sqlmock.NewResult(0, 1))
	appMock.ExpectExec("INSERT INTO organization_member_roles").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// nil roleTemplateID: the mirror keeps the row but holds no role -- migration
	// 000055 keeps "no mirrored row" and "mirrored row, no role" distinct.
	if err := m.UpdateMemberRoleTemplate(context.Background(), "org-1", "user-1", nil, idstore.OrgScopeOrganizations("org-1"), sw.reducer()); err != nil {
		t.Fatalf("UpdateMemberRoleTemplate: %v", err)
	}
	sw.wantsChanged(t, true)
}

// The IdP group-mapping reconcile calls UpdateMemberRole by NAME on every
// login. Before #491 that ended every OTHER session the user held each time
// they signed in anywhere, for as long as they were in a mapped organization.
func TestUpdateMemberRole_SameRoleNameDoesNotEndSessions(t *testing.T) {
	m, identityMock, appMock, done := twoConnections(t)
	defer done()
	var sw sweeps

	identityMock.ExpectQuery("(?s)FROM organization_members").
		WillReturnRows(sqlmock.NewRows([]string{
			"organization_id", "user_id", "role_template_id", "created_at",
			"user_name", "user_email",
			"role_template_name", "role_template_display_name", "role_template_scopes",
		}).AddRow("org-1", "user-1", templateID, time.Now(),
			"Alice", "alice@example.com",
			"editor", "Editor", []byte(`["states:read"]`)))
	// identity resolves the NAME to an id on its own connection, then updates.
	identityMock.ExpectQuery(regexp.QuoteMeta(`SELECT id FROM role_templates WHERE name = $1`)).
		WithArgs("editor").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(templateID))
	identityMock.ExpectExec("UPDATE organization_members").
		WillReturnResult(sqlmock.NewResult(0, 1))
	// then the mirror, on the app connection.
	appMock.ExpectQuery(regexp.QuoteMeta(`SELECT id FROM role_templates WHERE name = $1`)).
		WithArgs("editor").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(templateID))
	appMock.ExpectExec("INSERT INTO organization_member_roles").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := m.UpdateMemberRole(context.Background(), "org-1", "user-1", "editor", idstore.OrgScopeOrganizations("org-1"), sw.reducer()); err != nil {
		t.Fatalf("UpdateMemberRole: %v", err)
	}
	sw.wantsChanged(t, false)
}

// A genuine promotion or demotion on that same path must still end the sessions
// that froze the old authority.
func TestUpdateMemberRole_DifferentRoleNameEndsSessions(t *testing.T) {
	m, identityMock, appMock, done := twoConnections(t)
	defer done()
	var sw sweeps

	identityMock.ExpectQuery("(?s)FROM organization_members").
		WillReturnRows(sqlmock.NewRows([]string{
			"organization_id", "user_id", "role_template_id", "created_at",
			"user_name", "user_email",
			"role_template_name", "role_template_display_name", "role_template_scopes",
		}).AddRow("org-1", "user-1", templateID, time.Now(),
			"Alice", "alice@example.com",
			"viewer", "Viewer", []byte(`["states:read"]`)))
	// identity resolves the NAME to an id on its own connection, then updates.
	identityMock.ExpectQuery(regexp.QuoteMeta(`SELECT id FROM role_templates WHERE name = $1`)).
		WithArgs("editor").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(templateID))
	identityMock.ExpectExec("UPDATE organization_members").
		WillReturnResult(sqlmock.NewResult(0, 1))
	// then the mirror, on the app connection.
	appMock.ExpectQuery(regexp.QuoteMeta(`SELECT id FROM role_templates WHERE name = $1`)).
		WithArgs("editor").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(templateID))
	appMock.ExpectExec("INSERT INTO organization_member_roles").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := m.UpdateMemberRole(context.Background(), "org-1", "user-1", "editor", idstore.OrgScopeOrganizations("org-1"), sw.reducer()); err != nil {
		t.Fatalf("UpdateMemberRole: %v", err)
	}
	sw.wantsChanged(t, true)
}
