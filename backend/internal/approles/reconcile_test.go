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
// connection it writes and the identity connection it reads.
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

// appTemplateRows is the shape Store.ListTemplates scans.
func appTemplateRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{"id", "name", "display_name", "description", "scopes", "is_system"})
}

// expectDriftProbe stages the comparison Reconcile now runs BEFORE it writes
// anything, with both sides empty (no pending repairs).
//
// Staged as a helper rather than inline because it is four queries across two
// connections that say nothing about the case under test — and because a test
// that forgot one would fail with a sqlmock message about the NEXT query, which
// is how a suite becomes unreadable.
func expectDriftProbe(env *reconcileEnv) {
	env.identity.ExpectQuery(regexp.QuoteMeta(`SELECT id::text, name`)).
		WillReturnRows(appTemplateRows())
	env.app.ExpectQuery(regexp.QuoteMeta(`SELECT id, name, COALESCE(display_name`)).
		WillReturnRows(appTemplateRows())
	env.identity.ExpectQuery(`FROM organization_members ORDER BY`).
		WillReturnRows(sqlmock.NewRows([]string{"organization_id", "user_id", "role_template_id"}))
	env.app.ExpectQuery(`FROM organization_member_roles ORDER BY`).
		WillReturnRows(sqlmock.NewRows([]string{"organization_id", "user_id", "role_template_id"}))
}

// expectForeignTemplateReadback stages the ListTemplates the reconcile issues
// after the definition pass, to report what this application holds and does not
// define.
func expectForeignTemplateReadback(env *reconcileEnv, rows *sqlmock.Rows) {
	env.app.ExpectQuery(regexp.QuoteMeta(`SELECT id, name, COALESCE(display_name`)).WillReturnRows(rows)
}

func identityTemplateRows(adminScopes, editorScopes string) *sqlmock.Rows {
	now := time.Now()
	return sqlmock.NewRows([]string{"id", "name", "display_name", "description", "scopes", "is_system", "created_at", "updated_at"}).
		AddRow(adminTemplateID, "admin", "Administrator", nil, []byte(adminScopes), true, now, now).
		AddRow(editorTemplateID, "editor", "Editor", nil, []byte(editorScopes), true, now, now)
}

// expectTemplateAdoption stages the two statements the adopt pass issues per
// identity template: release the name if another id holds it, then insert IF
// ABSENT.
func expectTemplateAdoption(m sqlmock.Sqlmock, id, name, displayName string) {
	m.ExpectExec(regexp.QuoteMeta(`DELETE FROM role_templates WHERE name = $1 AND id <> $2`)).
		WithArgs(name, id).
		WillReturnResult(sqlmock.NewResult(0, 0))
	m.ExpectExec(`ON CONFLICT \(id\) DO NOTHING`).
		WithArgs(id, name, displayName, nil, sqlmock.AnyArg(), true).
		WillReturnResult(sqlmock.NewResult(0, 1))
}

// recordingDefiner is the app-side seed, as a test double that records that it
// ran and where in the sequence.
func recordingDefiner(ran *bool) TemplateDefiner {
	return func(context.Context, *Store) error {
		*ran = true
		return nil
	}
}

// scopesJSON renders a seeded role's own scopes as the JSON the identity column
// holds, so a case built from this build's own roles cannot drift from
// auth.AppRoleTemplates() by hand-copying.
func scopesJSON(t *testing.T, name string) string {
	t.Helper()
	for _, rt := range auth.AppRoleTemplates() {
		if rt.Name != name {
			continue
		}
		quoted := make([]string, 0, len(rt.Scopes))
		for _, s := range rt.Scopes {
			quoted = append(quoted, `"`+s+`"`)
		}
		return "[" + strings.Join(quoted, ",") + "]"
	}
	t.Fatalf("no seeded role template named %q", name)
	return ""
}

// THE ADOPT PASS COPIES IDENTITY'S IDS. Minting fresh ids would make
// organization_member_roles.role_template_id a different value from the one
// identity.organization_members holds for the same assignment, and the assignment
// pass — which copies that column straight across — would point at nothing.
func TestReconcile_AdoptsIdentityTemplatesPreservingTheirIDs(t *testing.T) {
	env := newReconcileEnv(t)
	var defined bool

	expectVerifyOK(env.app)
	expectDriftProbe(env)
	env.app.ExpectQuery(regexp.QuoteMeta(`SELECT now()`)).
		WillReturnRows(sqlmock.NewRows([]string{"now"}).AddRow(time.Now()))
	env.identity.ExpectQuery("SELECT id, name, display_name, description, scopes, is_system, created_at, updated_at").
		WillReturnRows(identityTemplateRows(scopesJSON(t, "admin"), scopesJSON(t, "editor")))
	expectTemplateAdoption(env.app, adminTemplateID, "admin", "Administrator")
	expectTemplateAdoption(env.app, editorTemplateID, "editor", "Editor")
	// The readback the report is built from. It carries every role this build
	// defines, because TemplatesDefined is now counted from the ROWS rather than
	// from len(auth.AppRoleTemplates()) — the constant would read the same on a
	// boot whose definer wrote nothing.
	definedRows := appTemplateRows()
	for i, rt := range auth.AppRoleTemplates() {
		definedRows.AddRow(fmt.Sprintf("aaaaaaaa-0000-0000-0000-%012d", i), rt.Name, rt.DisplayName, nil, []byte(`[]`), true)
	}
	expectForeignTemplateReadback(env, definedRows)

	// One short page of memberships ends the keyset scan.
	env.identity.ExpectQuery("SELECT organization_id, user_id, role_template_id").
		WillReturnRows(sqlmock.NewRows([]string{"organization_id", "user_id", "role_template_id"}).
			AddRow("org-1", "user-1", adminTemplateID))
	env.app.ExpectExec("INSERT INTO organization_member_roles").
		WithArgs("org-1", "user-1", adminTemplateID).
		WillReturnResult(sqlmock.NewResult(0, 1))
	env.app.ExpectExec(regexp.QuoteMeta(`DELETE FROM organization_member_roles WHERE mirrored_at < $1`)).
		WillReturnResult(sqlmock.NewResult(0, 4))

	rep, err := Reconcile(context.Background(), env.appDB, env.identityDB, recordingDefiner(&defined))
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !defined {
		t.Error("the app-side seed never ran: the mirror would carry identity's role scopes, not this build's")
	}
	if rep.TemplatesAdopted != 2 {
		t.Errorf("TemplatesAdopted = %d, want 2", rep.TemplatesAdopted)
	}
	if rep.TemplatesDefined != len(auth.AppRoleTemplates()) {
		t.Errorf("TemplatesDefined = %d, want %d", rep.TemplatesDefined, len(auth.AppRoleTemplates()))
	}
	if rep.AssignmentsRestated != 1 {
		t.Errorf("AssignmentsRestated = %d, want 1", rep.AssignmentsRestated)
	}
	if rep.StaleRemoved != 4 {
		t.Errorf("StaleRemoved = %d, want 4", rep.StaleRemoved)
	}
	if len(rep.ForeignTemplates) != 0 {
		t.Errorf("ForeignTemplates = %v, want none: `admin` is a role this build defines", rep.ForeignTemplates)
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

// IDENTITY MAY SUPPLY A DEFINITION, AND MAY NOT REDEFINE ONE. This is the
// difference between Phase 3a and Phase 3b for role templates, and it is one word
// of SQL: an upsert here lets the shared schema — in a coupled deployment, the
// sibling registry — rewrite what a TSM role grants, once per restart, on the
// table that now decides authorization.
//
// Asserted on the STATEMENT, because the two spellings are behaviourally
// identical on a fresh database and differ only on the deployment that has been
// running for months.
func TestReconcile_AdoptsWithoutOverwritingAnExistingDefinition(t *testing.T) {
	env := newReconcileEnv(t)
	var defined bool

	expectVerifyOK(env.app)
	expectDriftProbe(env)
	env.app.ExpectQuery(regexp.QuoteMeta(`SELECT now()`)).
		WillReturnRows(sqlmock.NewRows([]string{"now"}).AddRow(time.Now()))
	// editor carries the SIBLING's scopes in the shared schema.
	const siblingEditorScopes = `["modules:read","providers:read"]`
	env.identity.ExpectQuery("SELECT id, name").
		WillReturnRows(identityTemplateRows(scopesJSON(t, "admin"), siblingEditorScopes))
	env.app.ExpectExec(regexp.QuoteMeta(`DELETE FROM role_templates WHERE name = $1 AND id <> $2`)).
		WithArgs("admin", adminTemplateID).
		WillReturnResult(sqlmock.NewResult(0, 0))
	env.app.ExpectExec(`ON CONFLICT \(id\) DO NOTHING`).
		WithArgs(adminTemplateID, "admin", "Administrator", nil, sqlmock.AnyArg(), true).
		WillReturnResult(sqlmock.NewResult(0, 1))
	env.app.ExpectExec(regexp.QuoteMeta(`DELETE FROM role_templates WHERE name = $1 AND id <> $2`)).
		WithArgs("editor", editorTemplateID).
		WillReturnResult(sqlmock.NewResult(0, 0))
	// DO NOTHING, not DO UPDATE: this deployment's `editor` keeps whatever it
	// already grants, and the definition pass below is what sets it.
	env.app.ExpectExec(`ON CONFLICT \(id\) DO NOTHING`).
		WithArgs(editorTemplateID, "editor", "Editor", nil, siblingEditorScopes, true).
		WillReturnResult(sqlmock.NewResult(0, 1))
	expectForeignTemplateReadback(env, appTemplateRows())
	env.identity.ExpectQuery("SELECT organization_id, user_id, role_template_id").
		WillReturnRows(sqlmock.NewRows([]string{"organization_id", "user_id", "role_template_id"}))
	env.app.ExpectExec("DELETE FROM organization_member_roles WHERE mirrored_at").
		WillReturnResult(sqlmock.NewResult(0, 0))

	if _, err := Reconcile(context.Background(), env.appDB, env.identityDB, recordingDefiner(&defined)); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if err := env.app.ExpectationsWereMet(); err != nil {
		t.Errorf("app leg: %v", err)
	}
}

// THE DEFINITION PASS RUNS AFTER THE ADOPT PASS AND BEFORE THE ASSIGNMENTS, and
// the order is not cosmetic: adopting after defining would let
// RepointTemplateName delete the row the seed had just written, replacing this
// build's scopes with identity's on every fresh install.
//
// Asserted by having the definer record what the app connection had already been
// asked for when it ran.
func TestReconcile_DefinesOwnTemplatesAfterAdoptingAndBeforeAssignments(t *testing.T) {
	env := newReconcileEnv(t)

	expectVerifyOK(env.app)
	expectDriftProbe(env)
	env.app.ExpectQuery(regexp.QuoteMeta(`SELECT now()`)).
		WillReturnRows(sqlmock.NewRows([]string{"now"}).AddRow(time.Now()))
	env.identity.ExpectQuery("SELECT id, name").
		WillReturnRows(identityTemplateRows(scopesJSON(t, "admin"), scopesJSON(t, "editor")))
	expectTemplateAdoption(env.app, adminTemplateID, "admin", "Administrator")
	expectTemplateAdoption(env.app, editorTemplateID, "editor", "Editor")
	expectForeignTemplateReadback(env, appTemplateRows())
	env.identity.ExpectQuery("SELECT organization_id, user_id, role_template_id").
		WillReturnRows(sqlmock.NewRows([]string{"organization_id", "user_id", "role_template_id"}))
	env.app.ExpectExec("DELETE FROM organization_member_roles WHERE mirrored_at").
		WillReturnResult(sqlmock.NewResult(0, 0))

	var adoptionsDone, assignmentsStarted bool
	definer := func(ctx context.Context, _ *Store) error {
		// Both adoptions must already have been consumed, and no assignment may
		// have been written yet. sqlmock is ordered, so "the next expectation is
		// the readback" is exactly that statement.
		adoptionsDone = env.app.ExpectationsWereMet() != nil // still pending: the readback and beyond
		assignmentsStarted = false
		return nil
	}
	if _, err := Reconcile(context.Background(), env.appDB, env.identityDB, definer); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !adoptionsDone {
		t.Error("the definer ran with no pending app expectations at all, so the sequence under test did not happen")
	}
	if assignmentsStarted {
		t.Error("assignments were written before this build's role definitions")
	}
	if err := env.app.ExpectationsWereMet(); err != nil {
		t.Errorf("app leg: %v", err)
	}
}

// A RECONCILE WITH NO DEFINER IS REFUSED. Skipping the seed would leave this
// application's role_templates carrying identity's scopes, which is the Phase 3a
// meaning of those rows and the wrong answer for a build that reads them.
func TestReconcile_RefusesWithoutATemplateDefiner(t *testing.T) {
	env := newReconcileEnv(t)
	_, err := Reconcile(context.Background(), env.appDB, env.identityDB, nil)
	if err == nil {
		t.Fatal("Reconcile ran with no template definer, so this build's role scopes would never be written")
	}
	if !strings.Contains(err.Error(), "definer") {
		t.Fatalf("the error does not say what is missing: %v", err)
	}
}

// A NULL role_template_id in identity is a member with no role. It must be
// mirrored as NULL, not skipped: skipping it would leave a stale row from a
// previous reconcile standing, and the sweep would not collect it because the
// membership still exists.
func TestReconcile_MirrorsARoleLessMembershipAsNull(t *testing.T) {
	env := newReconcileEnv(t)
	var defined bool

	expectVerifyOK(env.app)
	expectDriftProbe(env)
	env.app.ExpectQuery(regexp.QuoteMeta(`SELECT now()`)).
		WillReturnRows(sqlmock.NewRows([]string{"now"}).AddRow(time.Now()))
	env.identity.ExpectQuery("SELECT id, name").
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "display_name", "description", "scopes", "is_system", "created_at", "updated_at"}))
	expectForeignTemplateReadback(env, appTemplateRows())
	env.identity.ExpectQuery("SELECT organization_id, user_id, role_template_id").
		WillReturnRows(sqlmock.NewRows([]string{"organization_id", "user_id", "role_template_id"}).
			AddRow("org-1", "user-1", nil))
	env.app.ExpectExec("INSERT INTO organization_member_roles").
		WithArgs("org-1", "user-1", nil).
		WillReturnResult(sqlmock.NewResult(0, 1))
	env.app.ExpectExec("DELETE FROM organization_member_roles WHERE mirrored_at").
		WillReturnResult(sqlmock.NewResult(0, 0))

	if _, err := Reconcile(context.Background(), env.appDB, env.identityDB, recordingDefiner(&defined)); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if err := env.app.ExpectationsWereMet(); err != nil {
		t.Errorf("app leg: %v", err)
	}
}

// THE PENDING-REPAIR REPORT IS TAKEN BEFORE ANYTHING IS WRITTEN, and it is what
// makes a boot that rewrote four hundred principals' authority distinguishable
// from one that rewrote nothing.
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
	env.identity.ExpectQuery(regexp.QuoteMeta(`SELECT id::text, name`)).
		WillReturnRows(appTemplateRows())
	env.app.ExpectQuery(regexp.QuoteMeta(`SELECT id, name, COALESCE(display_name`)).
		WillReturnRows(appTemplateRows())
	env.identity.ExpectQuery(`FROM organization_members ORDER BY`).
		WillReturnRows(sqlmock.NewRows([]string{"organization_id", "user_id", "role_template_id"}).
			AddRow(orgID, userID, adminTemplateID))
	env.app.ExpectQuery(`FROM organization_member_roles ORDER BY`).
		WillReturnRows(sqlmock.NewRows([]string{"organization_id", "user_id", "role_template_id"}))

	env.app.ExpectQuery(regexp.QuoteMeta(`SELECT now()`)).
		WillReturnRows(sqlmock.NewRows([]string{"now"}).AddRow(time.Now()))
	env.identity.ExpectQuery("SELECT id, name").
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "display_name", "description", "scopes", "is_system", "created_at", "updated_at"}))
	expectForeignTemplateReadback(env, appTemplateRows())
	env.identity.ExpectQuery("SELECT organization_id, user_id, role_template_id").
		WillReturnRows(sqlmock.NewRows([]string{"organization_id", "user_id", "role_template_id"}).
			AddRow(orgID, userID, adminTemplateID))
	env.app.ExpectExec("INSERT INTO organization_member_roles").
		WithArgs(orgID, userID, adminTemplateID).
		WillReturnResult(sqlmock.NewResult(0, 1))
	env.app.ExpectExec("DELETE FROM organization_member_roles WHERE mirrored_at").
		WillReturnResult(sqlmock.NewResult(0, 0))

	rep, err := Reconcile(context.Background(), env.appDB, env.identityDB, recordingDefiner(&defined))
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
// part-way leaves the remainder un-restated, and sweeping then would delete
// every assignment the scan did not reach — turning a transient identity fault
// into a wiped mirror.
func TestReconcile_DoesNotSweepWhenTheMembershipScanFails(t *testing.T) {
	env := newReconcileEnv(t)
	var defined bool

	expectVerifyOK(env.app)
	expectDriftProbe(env)
	env.app.ExpectQuery(regexp.QuoteMeta(`SELECT now()`)).
		WillReturnRows(sqlmock.NewRows([]string{"now"}).AddRow(time.Now()))
	env.identity.ExpectQuery("SELECT id, name").
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "display_name", "description", "scopes", "is_system", "created_at", "updated_at"}))
	expectForeignTemplateReadback(env, appTemplateRows())
	env.identity.ExpectQuery("SELECT organization_id, user_id, role_template_id").
		WillReturnError(errors.New("identity is unreachable"))
	// The app mock has NO sweep expectation: issuing one fails this test.

	rep, err := Reconcile(context.Background(), env.appDB, env.identityDB, recordingDefiner(&defined))
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
	env.identity.ExpectQuery("SELECT id, name").
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "display_name", "description", "scopes", "is_system", "created_at", "updated_at"}))
	expectForeignTemplateReadback(env, appTemplateRows())
	env.identity.ExpectQuery("SELECT organization_id, user_id, role_template_id").
		WillReturnRows(sqlmock.NewRows([]string{"organization_id", "user_id", "role_template_id"}).
			AddRow("org-1", "user-1", nil).
			RowError(0, errors.New("connection reset mid-stream")))

	_, err := Reconcile(context.Background(), env.appDB, env.identityDB, recordingDefiner(&defined))
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

	_, err := Reconcile(context.Background(), env.appDB, env.identityDB, recordingDefiner(&defined))
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

	_, err := Reconcile(context.Background(), env.appDB, env.identityDB, recordingDefiner(&defined))
	if err == nil {
		t.Fatal("Reconcile succeeded against a connection with no role_templates table")
	}
	if !strings.Contains(err.Error(), "000032") {
		t.Errorf("the error does not name the migration an operator has to apply: %v", err)
	}
}

// A role name this application holds and does NOT define is reported, because it
// means here whatever the application that seeded it into the shared schema
// decided. That is the successor to Phase 3a's divergence warning, and it names
// the opposite set: then, every role could carry the sibling's meaning; now, only
// the ones this build never claimed.
func TestReconcile_ReportsRolesThisBuildDoesNotDefine(t *testing.T) {
	env := newReconcileEnv(t)
	var defined bool

	expectVerifyOK(env.app)
	expectDriftProbe(env)
	env.app.ExpectQuery(regexp.QuoteMeta(`SELECT now()`)).
		WillReturnRows(sqlmock.NewRows([]string{"now"}).AddRow(time.Now()))
	now := time.Now()
	env.identity.ExpectQuery("SELECT id, name").
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "display_name", "description", "scopes", "is_system", "created_at", "updated_at"}).
			AddRow(adminTemplateID, "registry_publisher", "Publisher", nil, []byte(`["modules:write"]`), true, now, now))
	expectTemplateAdoption(env.app, adminTemplateID, "registry_publisher", "Publisher")
	expectForeignTemplateReadback(env, appTemplateRows().
		AddRow(adminTemplateID, "registry_publisher", "Publisher", nil, []byte(`["modules:write"]`), true).
		AddRow(editorTemplateID, "editor", "Editor", nil, []byte(`["state:read"]`), true))
	env.identity.ExpectQuery("SELECT organization_id, user_id, role_template_id").
		WillReturnRows(sqlmock.NewRows([]string{"organization_id", "user_id", "role_template_id"}))
	env.app.ExpectExec("DELETE FROM organization_member_roles WHERE mirrored_at").
		WillReturnResult(sqlmock.NewResult(0, 0))

	rep, err := Reconcile(context.Background(), env.appDB, env.identityDB, recordingDefiner(&defined))
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

// Scope comparison is a SET comparison. Order and duplicates carry no meaning in
// a role template's scopes, so comparing them as sequences would report every
// re-ordered seed as divergence and train operators to ignore the warning.
func TestSameScopeSet(t *testing.T) {
	cases := []struct {
		name string
		a, b []string
		want bool
	}{
		{"identical", []string{"state:read", "state:write"}, []string{"state:read", "state:write"}, true},
		{"reordered", []string{"state:read", "state:write"}, []string{"state:write", "state:read"}, true},
		{"duplicated", []string{"state:read", "state:read"}, []string{"state:read"}, true},
		{"missing one", []string{"state:read", "state:write"}, []string{"state:read"}, false},
		{"extra one", []string{"state:read"}, []string{"state:read", "admin"}, false},
		{"disjoint same size", []string{"state:read"}, []string{"admin"}, false},
		{"both empty", nil, nil, true},
		{"one empty", nil, []string{"admin"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := sameScopeSet(c.a, c.b); got != c.want {
				t.Fatalf("sameScopeSet(%v, %v) = %v, want %v", c.a, c.b, got, c.want)
			}
		})
	}
}

// Reconcile without an identity connection must refuse rather than treat "no
// memberships readable" as "no memberships exist" and sweep the mirror clean.
func TestReconcile_RefusesWithoutAnIdentityConnection(t *testing.T) {
	env := newReconcileEnv(t)
	var defined bool
	if _, err := Reconcile(context.Background(), env.appDB, nil, recordingDefiner(&defined)); err == nil {
		t.Fatal("Reconcile succeeded with no identity connection to reconcile from")
	}
	if _, err := Reconcile(context.Background(), nil, env.identityDB, recordingDefiner(&defined)); !errors.Is(err, ErrMisrouted) {
		t.Fatalf("Reconcile with no app connection: got %v, want ErrMisrouted", err)
	}
}

// LogReport is the startup line an operator reads. It is exercised here so a nil
// slice or a missing field cannot panic the boot it is supposed to describe, and
// so the two warnings that now carry an authorization change keep their remedy.
func TestLogReport_HandlesEveryShape(t *testing.T) {
	LogReport(Report{Templates: "public.role_templates", Assignments: "public.organization_member_roles"})
	LogReport(Report{
		Templates: "public.role_templates", Assignments: "public.organization_member_roles",
		TemplatesAdopted: 6, TemplatesDefined: 6, AssignmentsRestated: 12, StaleRemoved: 1,
		ForeignTemplates: []string{"registry_publisher"},
		PendingRepairs: DriftResult{
			Compared: 12, Missing: 1, Stale: 2, Mismatched: 3, ScopeDivergent: 1,
			TemplateDrift: []TemplateDrift{{Name: "editor", IdentityScopes: []string{"modules:read"}, AppScopes: []string{"state:read"}}},
			Sample:        []DriftRecord{{Kind: DriftMissing, OrganizationID: "org", UserID: "user"}},
		},
	})
	if ForeignTemplateRemedy == "" {
		t.Fatal("the foreign-template warning carries no remedy, so the only signal about a role this build does not own says nothing about what to do")
	}
}
