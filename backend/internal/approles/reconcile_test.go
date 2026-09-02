package approles

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"

	"github.com/terraform-state-manager/terraform-state-manager/internal/auth"
)

const (
	adminTemplateID  = "aaaaaaaa-0000-0000-0000-000000000001"
	editorTemplateID = "aaaaaaaa-0000-0000-0000-000000000002"
)

// reconcileEnv is the two-connection rig Reconcile runs against: the app
// connection it writes and the identity connection it reads memberships from.
//
// BOTH MOCKS ARE ORDERED AND STRICT, and that strictness is itself an
// assertion: since the adopt pass was retired, the ONLY identity statements a
// reconcile may issue are the drift probe's two reads and the membership scan.
// A reconcile that reached for identity.role_templates again would issue a
// query no test stages, and every test here would fail on it.
type reconcileEnv struct {
	appDB, identityDB *sql.DB
	app, identity     sqlmock.Sqlmock
}

func newReconcileEnv(t *testing.T) *reconcileEnv {
	t.Helper()
	appDB, appMock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New (app): %v", err)
	}
	identityDB, identityMock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New (identity): %v", err)
	}
	t.Cleanup(func() {
		_ = appDB.Close()
		_ = identityDB.Close()
	})
	return &reconcileEnv{appDB: appDB, identityDB: identityDB, app: appMock, identity: identityMock}
}

// expectVerifyOK stages a clean routing probe: identity's tables are NOT visible
// on the app connection, and both of TSM's own tables resolve.
func expectVerifyOK(m sqlmock.Sqlmock) {
	m.ExpectQuery(regexp.QuoteMeta(`SELECT n.nspname`)).
		WithArgs("organization_members").
		WillReturnRows(sqlmock.NewRows([]string{"to_regclass"}).AddRow(nil))
	m.ExpectQuery(regexp.QuoteMeta(`SELECT n.nspname`)).
		WithArgs("role_templates").
		WillReturnRows(sqlmock.NewRows([]string{"to_regclass"}).AddRow("public.role_templates"))
	m.ExpectQuery(regexp.QuoteMeta(`SELECT n.nspname`)).
		WithArgs("organization_member_roles").
		WillReturnRows(sqlmock.NewRows([]string{"to_regclass"}).AddRow("public.organization_member_roles"))
}

// appTemplateRows is the shape Store.ListTemplates scans. It carries the
// timestamps because that result is also what GET /admin/roles serves.
func appTemplateRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{"id", "name", "display_name", "description", "scopes", "is_system", "created_at", "updated_at"})
}

// expectDriftProbe stages the comparison Reconcile runs BEFORE it writes
// anything, with both sides empty (no pending repairs).
//
// TWO queries, one per connection, and none of the template reads the probe
// used to issue: CheckDrift stopped comparing role definitions when the
// identity.role_templates reads were retired.
func expectDriftProbe(env *reconcileEnv) {
	env.identity.ExpectQuery(`FROM organization_members ORDER BY`).
		WillReturnRows(sqlmock.NewRows([]string{"organization_id", "user_id", "role_template_id"}))
	env.app.ExpectQuery(`FROM organization_member_roles ORDER BY`).
		WillReturnRows(sqlmock.NewRows([]string{"organization_id", "user_id", "role_template_id"}))
}

// expectTemplatePreImage stages the ListTemplates the reconcile issues BEFORE
// it writes any definition (#557).
//
// It is a separate helper from the readback below even though the statement is
// identical, because the two reads answer different questions at different
// points in the sequence and the mocks are ordered: this one is the pre-image
// the narrowing detector compares against, and staging it in the wrong place
// would move the detection to the wrong side of the write. Rows given here are
// what the deployment ALREADY holds.
func expectTemplatePreImage(env *reconcileEnv, rows *sqlmock.Rows) {
	env.app.ExpectQuery(regexp.QuoteMeta(`SELECT id, name, COALESCE(display_name`)).WillReturnRows(rows)
}

// expectForeignTemplateReadback stages the ListTemplates the reconcile issues
// after the definition pass, to report what this application holds and does not
// define.
func expectForeignTemplateReadback(env *reconcileEnv, rows *sqlmock.Rows) {
	env.app.ExpectQuery(regexp.QuoteMeta(`SELECT id, name, COALESCE(display_name`)).WillReturnRows(rows)
}

// expectMembershipScan stages the identity keyset scan, which since the Phase 3
// close-out reads (organization_id, user_id) ONLY: the membership fact is the
// one thing identity still supplies to these tables.
func expectMembershipScan(env *reconcileEnv, rows *sqlmock.Rows) {
	env.identity.ExpectQuery(`SELECT organization_id, user_id\s+FROM organization_members\s+WHERE`).
		WillReturnRows(rows)
}

func membershipScanRows(pairs ...[2]string) *sqlmock.Rows {
	rows := sqlmock.NewRows([]string{"organization_id", "user_id"})
	for _, p := range pairs {
		rows.AddRow(p[0], p[1])
	}
	return rows
}

// expectConfirmMembership stages the presence-confirming upsert for one pair.
//
// THE STATEMENT SHAPE IS THE ASSERTION: the conflict arm may refresh
// mirrored_at and NOTHING ELSE. A reconcile whose upsert touched
// role_template_id would be identity restating this application's role policy
// again, which is exactly what this phase removed — so the regex pins the SET
// list to the end of the statement.
func expectConfirmMembership(env *reconcileEnv, orgID, userID string) {
	env.app.ExpectExec(`INSERT INTO organization_member_roles[\s\S]*DO UPDATE\s+SET mirrored_at = now\(\)\s*$`).
		WithArgs(orgID, userID).
		WillReturnResult(sqlmock.NewResult(0, 1))
}

// recordingDefiner is the app-side seed, as a test double that records that it
// ran and where in the sequence.
func recordingDefiner(ran *bool) TemplateDefiner {
	return func(context.Context) ([]Template, error) {
		*ran = true
		return buildDefinitions(), nil
	}
}

// buildDefinitions is this build's own role definitions, in the shape the real
// definer supplies them.
//
// It returns the REAL set rather than an empty slice, because since #557 the
// definitions the definer supplies are what the reconcile writes and what the
// foreign-template report measures "ours" against. A double that supplied
// nothing while the readback showed six roles describes a state the production
// code can no longer produce — the reconcile writes exactly what it was handed —
// and a fixture that models an impossible state tests nothing that can happen.
func buildDefinitions() []Template {
	seeds := auth.AppRoleTemplates()
	defs := make([]Template, 0, len(seeds))
	for _, rt := range seeds {
		description := rt.Description
		defs = append(defs, Template{
			Name: rt.Name, DisplayName: rt.DisplayName, Description: &description,
			Scopes: rt.Scopes, IsSystem: true,
		})
	}
	return defs
}

// expectBuildDefinitionWrites stages the one INSERT the reconcile issues per
// supplied definition.
func expectBuildDefinitionWrites(env *reconcileEnv) {
	for range auth.AppRoleTemplates() {
		env.app.ExpectExec(regexp.QuoteMeta(`INSERT INTO role_templates`)).
			WillReturnResult(sqlmock.NewResult(0, 1))
	}
}

// THE RECONCILE READS NOTHING FROM identity.role_templates. Both mocks are
// strict and ordered, so the proof is the sequence itself: define this build's
// own roles, read back what the table holds, confirm the membership facts, and
// sweep — with the identity connection asked for exactly the drift probe and
// the membership scan.
func TestReconcile_DefinesOwnRolesAndConfirmsMemberships(t *testing.T) {
	env := newReconcileEnv(t)
	var defined bool

	expectVerifyOK(env.app)
	expectDriftProbe(env)
	env.app.ExpectQuery(regexp.QuoteMeta(`SELECT now()`)).
		WillReturnRows(sqlmock.NewRows([]string{"now"}).AddRow(time.Now()))
	// The pre-image: this deployment holds no definitions yet, so this build's
	// definitions cannot be narrowing anything (#557), and every definition the
	// definer supplies is then written.
	expectTemplatePreImage(env, appTemplateRows())
	expectBuildDefinitionWrites(env)
	// The readback the report is built from. It carries every role this build
	// defines, because TemplatesDefined is counted from the ROWS rather than
	// from len(auth.AppRoleTemplates()) — the constant would read the same on a
	// boot whose definer wrote nothing.
	definedRows := appTemplateRows()
	for i, rt := range auth.AppRoleTemplates() {
		definedRows.AddRow(fmt.Sprintf("aaaaaaaa-0000-0000-0000-%012d", i), rt.Name, rt.DisplayName, nil, []byte(`[]`), true, time.Now(), time.Now())
	}
	expectForeignTemplateReadback(env, definedRows)

	// One short page of memberships ends the keyset scan.
	expectMembershipScan(env, membershipScanRows([2]string{"org-1", "user-1"}))
	expectConfirmMembership(env, "org-1", "user-1")
	env.app.ExpectExec(regexp.QuoteMeta(`DELETE FROM organization_member_roles WHERE mirrored_at < $1`)).
		WillReturnResult(sqlmock.NewResult(0, 4))

	rep, err := Reconcile(context.Background(), env.appDB, env.identityDB, recordingDefiner(&defined), NoTemplateAuthorityReduction)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !defined {
		t.Error("the app-side seed never ran: the mirror would carry stale role scopes, not this build's")
	}
	if rep.TemplatesDefined != len(auth.AppRoleTemplates()) {
		t.Errorf("TemplatesDefined = %d, want %d", rep.TemplatesDefined, len(auth.AppRoleTemplates()))
	}
	if rep.MembershipsConfirmed != 1 {
		t.Errorf("MembershipsConfirmed = %d, want 1", rep.MembershipsConfirmed)
	}
	if rep.StaleRemoved != 4 {
		t.Errorf("StaleRemoved = %d, want 4", rep.StaleRemoved)
	}
	if len(rep.ForeignTemplates) != 0 {
		t.Errorf("ForeignTemplates = %v, want none: every row in the readback is a role this build defines", rep.ForeignTemplates)
	}
	if rep.Templates != "public.role_templates" || rep.Assignments != "public.organization_member_roles" {
		t.Errorf("resolved names = %q/%q, want the public ones", rep.Templates, rep.Assignments)
	}
	if err := env.app.ExpectationsWereMet(); err != nil {
		t.Errorf("app leg: %v", err)
	}
	if err := env.identity.ExpectationsWereMet(); err != nil {
		t.Errorf("identity leg: %v", err)
	}
}

// THE DEFINITION PASS RUNS BEFORE THE MEMBERSHIP SCAN. Templates first because
// organization_member_roles.role_template_id has a real foreign key to them;
// asserted by having the definer observe that no membership work had been
// staged-and-consumed when it ran (sqlmock is ordered, so pending expectations
// are exactly "what has not happened yet").
func TestReconcile_DefinesOwnTemplatesBeforeMemberships(t *testing.T) {
	env := newReconcileEnv(t)

	expectVerifyOK(env.app)
	expectDriftProbe(env)
	env.app.ExpectQuery(regexp.QuoteMeta(`SELECT now()`)).
		WillReturnRows(sqlmock.NewRows([]string{"now"}).AddRow(time.Now()))
	// The pre-image: this deployment holds no definitions yet, so this build's
	// definitions cannot be narrowing anything (#557), and every definition the
	// definer supplies is then written.
	expectTemplatePreImage(env, appTemplateRows())
	expectBuildDefinitionWrites(env)
	expectForeignTemplateReadback(env, appTemplateRows())
	expectMembershipScan(env, membershipScanRows())
	env.app.ExpectExec("DELETE FROM organization_member_roles WHERE mirrored_at").
		WillReturnResult(sqlmock.NewResult(0, 0))

	var definerSawPendingWork bool
	definer := func(context.Context) ([]Template, error) {
		// The readback, the scan and the sweep must all still be pending.
		definerSawPendingWork = env.app.ExpectationsWereMet() != nil
		return buildDefinitions(), nil
	}
	if _, err := Reconcile(context.Background(), env.appDB, env.identityDB, definer, NoTemplateAuthorityReduction); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !definerSawPendingWork {
		t.Error("the definer ran with no pending app expectations at all, so the sequence under test did not happen")
	}
	if err := env.app.ExpectationsWereMet(); err != nil {
		t.Errorf("app leg: %v", err)
	}
}

// A RECONCILE WITH NO DEFINER IS REFUSED. Skipping the seed would leave this
// application's role_templates carrying whatever an earlier build wrote, which
// is the wrong answer for a build that reads them.
func TestReconcile_RefusesWithoutATemplateDefiner(t *testing.T) {
	env := newReconcileEnv(t)
	_, err := Reconcile(context.Background(), env.appDB, env.identityDB, nil, NoTemplateAuthorityReduction)
	if err == nil {
		t.Fatal("Reconcile ran with no template definer, so this build's role scopes would never be written")
	}
	if !strings.Contains(err.Error(), "definer") {
		t.Fatalf("the error does not say what is missing: %v", err)
	}
}

// CONFIRMING A MEMBERSHIP DOES NOT TOUCH ITS ROLE. The scan reads only
// (organization_id, user_id) from identity — role_template_id is deliberately
// absent from the SELECT — and the upsert's conflict arm refreshes mirrored_at
// alone, so a role this application granted survives every boot and identity's
// opinion of the role is never restated over it.
func TestReconcile_ConfirmsPresenceWithoutRestatingTheRole(t *testing.T) {
	env := newReconcileEnv(t)
	var defined bool

	expectVerifyOK(env.app)
	expectDriftProbe(env)
	env.app.ExpectQuery(regexp.QuoteMeta(`SELECT now()`)).
		WillReturnRows(sqlmock.NewRows([]string{"now"}).AddRow(time.Now()))
	// The pre-image: this deployment holds no definitions yet, so this build's
	// definitions cannot be narrowing anything (#557), and every definition the
	// definer supplies is then written.
	expectTemplatePreImage(env, appTemplateRows())
	expectBuildDefinitionWrites(env)
	expectForeignTemplateReadback(env, appTemplateRows())
	expectMembershipScan(env, membershipScanRows([2]string{"org-1", "user-1"}))
	// expectConfirmMembership pins the statement shape: WithArgs carries NO
	// role value at all, and the DO UPDATE arm ends at mirrored_at.
	expectConfirmMembership(env, "org-1", "user-1")
	env.app.ExpectExec("DELETE FROM organization_member_roles WHERE mirrored_at").
		WillReturnResult(sqlmock.NewResult(0, 0))

	if _, err := Reconcile(context.Background(), env.appDB, env.identityDB, recordingDefiner(&defined), NoTemplateAuthorityReduction); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if err := env.app.ExpectationsWereMet(); err != nil {
		t.Errorf("app leg: %v", err)
	}
	if err := env.identity.ExpectationsWereMet(); err != nil {
		t.Errorf("identity leg: %v", err)
	}
}

// THE PENDING-REPAIR REPORT IS TAKEN BEFORE ANYTHING IS WRITTEN, and it is what
// makes a boot that changed principals' records distinguishable from one that
// changed nothing.
//
// Asserted on the exact counts and the exact record, not on "the field is
// non-empty": a comparison taken AFTER the passes would always report zero, and
// would still satisfy a presence check.
func TestReconcile_ReportsWhatItIsAboutToRepair(t *testing.T) {
	env := newReconcileEnv(t)
	var defined bool
	const orgID = "11111111-0000-0000-0000-000000000001"
	const userID = "22222222-0000-0000-0000-000000000001"

	expectVerifyOK(env.app)
	// Identity has a membership this application records no role for: a principal
	// who has LOST access they should have.
	env.identity.ExpectQuery(`FROM organization_members ORDER BY`).
		WillReturnRows(sqlmock.NewRows([]string{"organization_id", "user_id", "role_template_id"}).
			AddRow(orgID, userID, adminTemplateID))
	env.app.ExpectQuery(`FROM organization_member_roles ORDER BY`).
		WillReturnRows(sqlmock.NewRows([]string{"organization_id", "user_id", "role_template_id"}))

	env.app.ExpectQuery(regexp.QuoteMeta(`SELECT now()`)).
		WillReturnRows(sqlmock.NewRows([]string{"now"}).AddRow(time.Now()))
	// The pre-image: this deployment holds no definitions yet, so this build's
	// definitions cannot be narrowing anything (#557), and every definition the
	// definer supplies is then written.
	expectTemplatePreImage(env, appTemplateRows())
	expectBuildDefinitionWrites(env)
	expectForeignTemplateReadback(env, appTemplateRows())
	expectMembershipScan(env, membershipScanRows([2]string{orgID, userID}))
	expectConfirmMembership(env, orgID, userID)
	env.app.ExpectExec("DELETE FROM organization_member_roles WHERE mirrored_at").
		WillReturnResult(sqlmock.NewResult(0, 0))

	rep, err := Reconcile(context.Background(), env.appDB, env.identityDB, recordingDefiner(&defined), NoTemplateAuthorityReduction)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if rep.PendingRepairs.Missing != 1 {
		t.Errorf("PendingRepairs.Missing = %d, want 1", rep.PendingRepairs.Missing)
	}
	if rep.PendingRepairs.Stale != 0 || rep.PendingRepairs.Mismatched != 0 {
		t.Errorf("PendingRepairs = %+v, want only one missing record", rep.PendingRepairs)
	}
	if got := rep.PendingRepairs.AssignmentDrift(); got != 1 {
		t.Errorf("AssignmentDrift() = %d, want 1", got)
	}
	if len(rep.PendingRepairs.Sample) != 1 {
		t.Fatalf("Sample = %v, want exactly one record naming the pair", rep.PendingRepairs.Sample)
	}
	s := rep.PendingRepairs.Sample[0]
	if s.Kind != DriftMissing || s.OrganizationID != orgID || s.UserID != userID || s.IdentityRole != adminTemplateID {
		t.Errorf("Sample[0] = %+v, want a `missing` record naming org=%s user=%s role=%s", s, orgID, userID, adminTemplateID)
	}
}

// THE SWEEP MUST NOT RUN ON A PARTIAL PASS. A membership scan that failed
// part-way leaves the remainder unconfirmed, and sweeping then would delete
// every assignment the scan did not reach — turning a transient identity fault
// into a wiped mirror.
func TestReconcile_DoesNotSweepWhenTheMembershipScanFails(t *testing.T) {
	env := newReconcileEnv(t)
	var defined bool

	expectVerifyOK(env.app)
	expectDriftProbe(env)
	env.app.ExpectQuery(regexp.QuoteMeta(`SELECT now()`)).
		WillReturnRows(sqlmock.NewRows([]string{"now"}).AddRow(time.Now()))
	// The pre-image: this deployment holds no definitions yet, so this build's
	// definitions cannot be narrowing anything (#557), and every definition the
	// definer supplies is then written.
	expectTemplatePreImage(env, appTemplateRows())
	expectBuildDefinitionWrites(env)
	expectForeignTemplateReadback(env, appTemplateRows())
	env.identity.ExpectQuery(`SELECT organization_id, user_id\s+FROM organization_members\s+WHERE`).
		WillReturnError(errors.New("identity is unreachable"))
	// The app mock has NO sweep expectation: issuing one fails this test.

	rep, err := Reconcile(context.Background(), env.appDB, env.identityDB, recordingDefiner(&defined), NoTemplateAuthorityReduction)
	if err == nil {
		t.Fatal("Reconcile reported success despite an unreadable identity membership scan")
	}
	if !strings.Contains(err.Error(), "identity is unreachable") {
		t.Fatalf("the underlying failure was not reported: %v", err)
	}
	if rep.StaleRemoved != 0 {
		t.Fatalf("StaleRemoved = %d on a failed pass, want 0", rep.StaleRemoved)
	}
	if err := env.app.ExpectationsWereMet(); err != nil {
		t.Errorf("app leg: %v", err)
	}
}

// A ROW ERROR MID-STREAM IS THE SAME HAZARD, and is the one a `for rows.Next()`
// loop swallows if rows.Err() is not consulted: the loop simply stops early and
// the caller sees a short, successful pass.
func TestReconcile_DoesNotSweepWhenTheMembershipStreamBreaksMidway(t *testing.T) {
	env := newReconcileEnv(t)
	var defined bool

	expectVerifyOK(env.app)
	expectDriftProbe(env)
	env.app.ExpectQuery(regexp.QuoteMeta(`SELECT now()`)).
		WillReturnRows(sqlmock.NewRows([]string{"now"}).AddRow(time.Now()))
	// The pre-image: this deployment holds no definitions yet, so this build's
	// definitions cannot be narrowing anything (#557), and every definition the
	// definer supplies is then written.
	expectTemplatePreImage(env, appTemplateRows())
	expectBuildDefinitionWrites(env)
	expectForeignTemplateReadback(env, appTemplateRows())
	expectMembershipScan(env, membershipScanRows([2]string{"org-1", "user-1"}).
		RowError(0, errors.New("connection reset mid-stream")))

	_, err := Reconcile(context.Background(), env.appDB, env.identityDB, recordingDefiner(&defined), NoTemplateAuthorityReduction)
	if err == nil {
		t.Fatal("Reconcile reported success despite a membership stream that broke midway")
	}
	if !strings.Contains(err.Error(), "connection reset mid-stream") {
		t.Fatalf("the row error was swallowed: %v", err)
	}
	if err := env.app.ExpectationsWereMet(); err != nil {
		t.Errorf("app leg: %v", err)
	}
}

// A misrouted app connection must ABORT, not mirror into the shared identity
// table. This is the one failure mode that looks identical to success in every
// other observable, and it is the exact collision this phase exists to end.
func TestReconcile_RefusesAnAppConnectionRoutedIntoIdentity(t *testing.T) {
	env := newReconcileEnv(t)
	var defined bool

	env.app.ExpectQuery(regexp.QuoteMeta(`SELECT n.nspname`)).
		WithArgs("organization_members").
		WillReturnRows(sqlmock.NewRows([]string{"to_regclass"}).AddRow("identity.organization_members"))
	env.app.ExpectQuery(regexp.QuoteMeta(`SHOW search_path`)).
		WillReturnRows(sqlmock.NewRows([]string{"search_path"}).AddRow("identity, public"))

	_, err := Reconcile(context.Background(), env.appDB, env.identityDB, recordingDefiner(&defined), NoTemplateAuthorityReduction)
	if !errors.Is(err, ErrMisrouted) {
		t.Fatalf("expected ErrMisrouted, got %v", err)
	}
	if !strings.Contains(err.Error(), "identity.organization_members") {
		t.Errorf("the error does not name what it resolved: %v", err)
	}
	if defined {
		t.Error("this build's role definitions were written into a connection that resolves identity's tables")
	}
	if err := env.app.ExpectationsWereMet(); err != nil {
		t.Errorf("app leg: %v", err)
	}
}

// A missing table is a migration that has not been applied, and it must abort
// startup rather than let the mirror silently write nothing.
func TestReconcile_RefusesWhenTheMigrationHasNotRun(t *testing.T) {
	env := newReconcileEnv(t)
	var defined bool

	env.app.ExpectQuery(regexp.QuoteMeta(`SELECT n.nspname`)).
		WithArgs("organization_members").
		WillReturnRows(sqlmock.NewRows([]string{"to_regclass"}).AddRow(nil))
	env.app.ExpectQuery(regexp.QuoteMeta(`SELECT n.nspname`)).
		WithArgs("role_templates").
		WillReturnRows(sqlmock.NewRows([]string{"to_regclass"}).AddRow(nil))

	_, err := Reconcile(context.Background(), env.appDB, env.identityDB, recordingDefiner(&defined), NoTemplateAuthorityReduction)
	if err == nil {
		t.Fatal("Reconcile succeeded against a connection with no role_templates table")
	}
	if !strings.Contains(err.Error(), "000032") {
		t.Errorf("the error does not name the migration an operator has to apply: %v", err)
	}
}

// A role name this application holds and does NOT define is reported, because it
// means here whatever the application that seeded it into the shared schema
// decided when an earlier build adopted it. No NEW foreign role can arrive —
// the adopt pass is retired — so the set can only shrink, and this report is
// how an operator watches it do so.
func TestReconcile_ReportsRolesThisBuildDoesNotDefine(t *testing.T) {
	env := newReconcileEnv(t)
	var defined bool

	expectVerifyOK(env.app)
	expectDriftProbe(env)
	env.app.ExpectQuery(regexp.QuoteMeta(`SELECT now()`)).
		WillReturnRows(sqlmock.NewRows([]string{"now"}).AddRow(time.Now()))
	// The pre-image: this deployment holds no definitions yet, so this build's
	// definitions cannot be narrowing anything (#557), and every definition the
	// definer supplies is then written.
	expectTemplatePreImage(env, appTemplateRows())
	expectBuildDefinitionWrites(env)
	// The table already holds a row an earlier build adopted from the shared
	// schema, alongside one role this build defines.
	expectForeignTemplateReadback(env, appTemplateRows().
		AddRow(adminTemplateID, "registry_publisher", "Publisher", nil, []byte(`["modules:write"]`), true, time.Now(), time.Now()).
		AddRow(editorTemplateID, "editor", "Editor", nil, []byte(`["state:read"]`), true, time.Now(), time.Now()))
	expectMembershipScan(env, membershipScanRows())
	env.app.ExpectExec("DELETE FROM organization_member_roles WHERE mirrored_at").
		WillReturnResult(sqlmock.NewResult(0, 0))

	rep, err := Reconcile(context.Background(), env.appDB, env.identityDB, recordingDefiner(&defined), NoTemplateAuthorityReduction)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(rep.ForeignTemplates) != 1 || rep.ForeignTemplates[0] != "registry_publisher" {
		t.Fatalf("ForeignTemplates = %v, want [registry_publisher]: `editor` is a role this build defines", rep.ForeignTemplates)
	}
	// COUNTED FROM THE ROWS, NOT FROM auth.AppRoleTemplates(). The readback above
	// holds exactly one role this build defines, so a report that said 6 here would
	// be describing the intent — and would read the same on a boot whose definer
	// wrote nothing at all, which is precisely the boot an operator needs the
	// startup line to distinguish.
	if rep.TemplatesDefined != 1 {
		t.Fatalf("TemplatesDefined = %d, want 1: the table holds one role this build defines, whatever "+
			"auth.AppRoleTemplates() lists", rep.TemplatesDefined)
	}
}

// Reconcile without an identity connection must refuse rather than treat "no
// memberships readable" as "no memberships exist" and sweep the mirror clean.
func TestReconcile_RefusesWithoutAnIdentityConnection(t *testing.T) {
	env := newReconcileEnv(t)
	var defined bool
	if _, err := Reconcile(context.Background(), env.appDB, nil, recordingDefiner(&defined), NoTemplateAuthorityReduction); err == nil {
		t.Fatal("Reconcile succeeded with no identity connection to reconcile from")
	}
	if _, err := Reconcile(context.Background(), nil, env.identityDB, recordingDefiner(&defined), NoTemplateAuthorityReduction); !errors.Is(err, ErrMisrouted) {
		t.Fatalf("Reconcile with no app connection: got %v, want ErrMisrouted", err)
	}
}

// LogReport is the startup line an operator reads. It is exercised here so a nil
// slice or a missing field cannot panic the boot it is supposed to describe, and
// so the warning that carries an authorization decision keeps its remedy.
func TestLogReport_HandlesEveryShape(t *testing.T) {
	LogReport(Report{Templates: "public.role_templates", Assignments: "public.organization_member_roles"})
	LogReport(Report{
		Templates: "public.role_templates", Assignments: "public.organization_member_roles",
		TemplatesDefined: 6, MembershipsConfirmed: 12, StaleRemoved: 1,
		ForeignTemplates: []string{"registry_publisher"},
		PendingRepairs: DriftResult{
			Compared: 12, Missing: 1, Stale: 2, Mismatched: 3,
			Sample: []DriftRecord{{Kind: DriftMissing, OrganizationID: "org", UserID: "user"}},
		},
	})
	if ForeignTemplateRemedy == "" {
		t.Fatal("the foreign-template warning carries no remedy, so the only signal about a role this build does not own says nothing about what to do")
	}
}
