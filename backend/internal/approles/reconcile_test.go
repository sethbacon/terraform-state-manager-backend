package approles

import (
	"context"
	"database/sql"
	"errors"
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

func identityTemplateRows(adminScopes, editorScopes string) *sqlmock.Rows {
	now := time.Now()
	return sqlmock.NewRows([]string{"id", "name", "display_name", "description", "scopes", "is_system", "created_at", "updated_at"}).
		AddRow(adminTemplateID, "admin", "Administrator", nil, []byte(adminScopes), true, now, now).
		AddRow(editorTemplateID, "editor", "Editor", nil, []byte(editorScopes), true, now, now)
}

func expectTemplateCopy(m sqlmock.Sqlmock, id, name, displayName string) {
	m.ExpectExec(regexp.QuoteMeta(`DELETE FROM role_templates WHERE name = $1 AND id <> $2`)).
		WithArgs(name, id).
		WillReturnResult(sqlmock.NewResult(0, 0))
	m.ExpectExec("INSERT INTO role_templates").
		WithArgs(id, name, displayName, nil, sqlmock.AnyArg(), true).
		WillReturnResult(sqlmock.NewResult(0, 1))
}

// scopesJSON renders a seeded role's own scopes as the JSON the identity column
// holds, so the "no divergence" case is built from the SAME source the
// divergence check compares against and cannot drift from it by hand-copying.
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

// THE BACKFILL COPIES IDENTITY'S ROWS, INCLUDING THEIR IDS. Minting fresh ids
// would make organization_member_roles.role_template_id a different value from
// the one identity.organization_members holds for the same assignment, and the
// assignment pass — which copies that column straight across — would point at
// nothing.
func TestReconcile_CopiesIdentityTemplatesPreservingTheirIDs(t *testing.T) {
	env := newReconcileEnv(t)

	expectVerifyOK(env.app)
	env.app.ExpectQuery(regexp.QuoteMeta(`SELECT now()`)).
		WillReturnRows(sqlmock.NewRows([]string{"now"}).AddRow(time.Now()))
	env.identity.ExpectQuery("SELECT id, name, display_name, description, scopes, is_system, created_at, updated_at").
		WillReturnRows(identityTemplateRows(scopesJSON(t, "admin"), scopesJSON(t, "editor")))
	expectTemplateCopy(env.app, adminTemplateID, "admin", "Administrator")
	expectTemplateCopy(env.app, editorTemplateID, "editor", "Editor")

	// One short page of memberships ends the keyset scan.
	env.identity.ExpectQuery("SELECT organization_id, user_id, role_template_id").
		WillReturnRows(sqlmock.NewRows([]string{"organization_id", "user_id", "role_template_id"}).
			AddRow("org-1", "user-1", adminTemplateID))
	env.app.ExpectExec("INSERT INTO organization_member_roles").
		WithArgs("org-1", "user-1", adminTemplateID).
		WillReturnResult(sqlmock.NewResult(0, 1))
	env.app.ExpectExec(regexp.QuoteMeta(`DELETE FROM organization_member_roles WHERE mirrored_at < $1`)).
		WillReturnResult(sqlmock.NewResult(0, 4))

	rep, err := Reconcile(context.Background(), env.appDB, env.identityDB)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if rep.TemplatesCopied != 2 {
		t.Errorf("TemplatesCopied = %d, want 2", rep.TemplatesCopied)
	}
	if rep.AssignmentsRestated != 1 {
		t.Errorf("AssignmentsRestated = %d, want 1", rep.AssignmentsRestated)
	}
	if rep.StaleRemoved != 4 {
		t.Errorf("StaleRemoved = %d, want 4", rep.StaleRemoved)
	}
	if len(rep.DivergentTemplates) != 0 {
		t.Errorf("DivergentTemplates = %v, want none: the rows carry this build's own scopes", rep.DivergentTemplates)
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

// A NULL role_template_id in identity is a member with no role. It must be
// mirrored as NULL, not skipped: skipping it would leave a stale row from a
// previous reconcile standing, and the sweep would not collect it because the
// membership still exists.
func TestReconcile_MirrorsARoleLessMembershipAsNull(t *testing.T) {
	env := newReconcileEnv(t)

	expectVerifyOK(env.app)
	env.app.ExpectQuery(regexp.QuoteMeta(`SELECT now()`)).
		WillReturnRows(sqlmock.NewRows([]string{"now"}).AddRow(time.Now()))
	env.identity.ExpectQuery("SELECT id, name").
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "display_name", "description", "scopes", "is_system", "created_at", "updated_at"}))
	env.identity.ExpectQuery("SELECT organization_id, user_id, role_template_id").
		WillReturnRows(sqlmock.NewRows([]string{"organization_id", "user_id", "role_template_id"}).
			AddRow("org-1", "user-1", nil))
	env.app.ExpectExec("INSERT INTO organization_member_roles").
		WithArgs("org-1", "user-1", nil).
		WillReturnResult(sqlmock.NewResult(0, 1))
	env.app.ExpectExec("DELETE FROM organization_member_roles WHERE mirrored_at").
		WillReturnResult(sqlmock.NewResult(0, 0))

	if _, err := Reconcile(context.Background(), env.appDB, env.identityDB); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if err := env.app.ExpectationsWereMet(); err != nil {
		t.Errorf("app leg: %v", err)
	}
}

// THE SWEEP MUST NOT RUN ON A PARTIAL PASS. A membership scan that failed
// part-way leaves the remainder un-restated, and sweeping then would delete
// every assignment the scan did not reach — turning a transient identity fault
// into a wiped mirror.
func TestReconcile_DoesNotSweepWhenTheMembershipScanFails(t *testing.T) {
	env := newReconcileEnv(t)

	expectVerifyOK(env.app)
	env.app.ExpectQuery(regexp.QuoteMeta(`SELECT now()`)).
		WillReturnRows(sqlmock.NewRows([]string{"now"}).AddRow(time.Now()))
	env.identity.ExpectQuery("SELECT id, name").
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "display_name", "description", "scopes", "is_system", "created_at", "updated_at"}))
	env.identity.ExpectQuery("SELECT organization_id, user_id, role_template_id").
		WillReturnError(errors.New("identity is unreachable"))
	// The app mock has NO sweep expectation: issuing one fails this test.

	rep, err := Reconcile(context.Background(), env.appDB, env.identityDB)
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

	expectVerifyOK(env.app)
	env.app.ExpectQuery(regexp.QuoteMeta(`SELECT now()`)).
		WillReturnRows(sqlmock.NewRows([]string{"now"}).AddRow(time.Now()))
	env.identity.ExpectQuery("SELECT id, name").
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "display_name", "description", "scopes", "is_system", "created_at", "updated_at"}))
	env.identity.ExpectQuery("SELECT organization_id, user_id, role_template_id").
		WillReturnRows(sqlmock.NewRows([]string{"organization_id", "user_id", "role_template_id"}).
			AddRow("org-1", "user-1", nil).
			RowError(0, errors.New("connection reset mid-stream")))

	_, err := Reconcile(context.Background(), env.appDB, env.identityDB)
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

	env.app.ExpectQuery(regexp.QuoteMeta(`SELECT n.nspname`)).
		WithArgs("organization_members").
		WillReturnRows(sqlmock.NewRows([]string{"to_regclass"}).AddRow("identity.organization_members"))
	env.app.ExpectQuery(regexp.QuoteMeta(`SHOW search_path`)).
		WillReturnRows(sqlmock.NewRows([]string{"search_path"}).AddRow("identity, public"))

	_, err := Reconcile(context.Background(), env.appDB, env.identityDB)
	if !errors.Is(err, ErrMisrouted) {
		t.Fatalf("expected ErrMisrouted, got %v", err)
	}
	if !strings.Contains(err.Error(), "identity.organization_members") {
		t.Errorf("the error does not name what it resolved: %v", err)
	}
	if err := env.app.ExpectationsWereMet(); err != nil {
		t.Errorf("app leg: %v", err)
	}
}

// A missing table is a migration that has not been applied, and it must abort
// startup rather than let the mirror silently write nothing.
func TestReconcile_RefusesWhenTheMigrationHasNotRun(t *testing.T) {
	env := newReconcileEnv(t)

	env.app.ExpectQuery(regexp.QuoteMeta(`SELECT n.nspname`)).
		WithArgs("organization_members").
		WillReturnRows(sqlmock.NewRows([]string{"to_regclass"}).AddRow(nil))
	env.app.ExpectQuery(regexp.QuoteMeta(`SELECT n.nspname`)).
		WithArgs("role_templates").
		WillReturnRows(sqlmock.NewRows([]string{"to_regclass"}).AddRow(nil))

	_, err := Reconcile(context.Background(), env.appDB, env.identityDB)
	if err == nil {
		t.Fatal("Reconcile succeeded against a connection with no role_templates table")
	}
	if !strings.Contains(err.Error(), "000031") {
		t.Errorf("the error does not name the migration an operator has to apply: %v", err)
	}
}

// DIVERGENCE IS REPORTED, NOT CORRECTED. A coupled deployment authorizes against
// the sibling app's definition of a role name today; Phase 3a must mirror that
// AS IT IS, because rewriting it to this build's scopes would change
// authorization in a phase that is required to change nothing.
func TestReconcile_ReportsDivergentScopesWithoutRewritingThem(t *testing.T) {
	env := newReconcileEnv(t)

	expectVerifyOK(env.app)
	env.app.ExpectQuery(regexp.QuoteMeta(`SELECT now()`)).
		WillReturnRows(sqlmock.NewRows([]string{"now"}).AddRow(time.Now()))
	// editor carries the SIBLING's scopes here, not this build's.
	const siblingEditorScopes = `["modules:read","providers:read"]`
	env.identity.ExpectQuery("SELECT id, name").
		WillReturnRows(identityTemplateRows(scopesJSON(t, "admin"), siblingEditorScopes))
	env.app.ExpectExec(regexp.QuoteMeta(`DELETE FROM role_templates WHERE name = $1 AND id <> $2`)).
		WithArgs("admin", adminTemplateID).
		WillReturnResult(sqlmock.NewResult(0, 0))
	env.app.ExpectExec("INSERT INTO role_templates").
		WithArgs(adminTemplateID, "admin", "Administrator", nil, sqlmock.AnyArg(), true).
		WillReturnResult(sqlmock.NewResult(0, 1))
	env.app.ExpectExec(regexp.QuoteMeta(`DELETE FROM role_templates WHERE name = $1 AND id <> $2`)).
		WithArgs("editor", editorTemplateID).
		WillReturnResult(sqlmock.NewResult(0, 0))
	// THE SIBLING'S SCOPES ARE WHAT IS WRITTEN. Asserted on the argument, so a
	// "helpful" implementation that substituted auth.AppRoleTemplates() here
	// fails rather than silently changing this deployment's authorization.
	env.app.ExpectExec("INSERT INTO role_templates").
		WithArgs(editorTemplateID, "editor", "Editor", nil, siblingEditorScopes, true).
		WillReturnResult(sqlmock.NewResult(0, 1))
	env.identity.ExpectQuery("SELECT organization_id, user_id, role_template_id").
		WillReturnRows(sqlmock.NewRows([]string{"organization_id", "user_id", "role_template_id"}))
	env.app.ExpectExec("DELETE FROM organization_member_roles WHERE mirrored_at").
		WillReturnResult(sqlmock.NewResult(0, 0))

	rep, err := Reconcile(context.Background(), env.appDB, env.identityDB)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(rep.DivergentTemplates) != 1 || rep.DivergentTemplates[0] != "editor" {
		t.Fatalf("DivergentTemplates = %v, want [editor]", rep.DivergentTemplates)
	}
	if err := env.app.ExpectationsWereMet(); err != nil {
		t.Errorf("app leg: %v", err)
	}
}

// A role template identity has that this build does not define is NOT
// divergence: the divergence report is about names this build claims, and
// reporting every foreign role would drown the signal that matters.
func TestReconcile_DoesNotReportRolesThisBuildDoesNotDefine(t *testing.T) {
	env := newReconcileEnv(t)

	expectVerifyOK(env.app)
	env.app.ExpectQuery(regexp.QuoteMeta(`SELECT now()`)).
		WillReturnRows(sqlmock.NewRows([]string{"now"}).AddRow(time.Now()))
	now := time.Now()
	env.identity.ExpectQuery("SELECT id, name").
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "display_name", "description", "scopes", "is_system", "created_at", "updated_at"}).
			AddRow(adminTemplateID, "registry_publisher", "Publisher", nil, []byte(`["modules:write"]`), true, now, now))
	expectTemplateCopy(env.app, adminTemplateID, "registry_publisher", "Publisher")
	env.identity.ExpectQuery("SELECT organization_id, user_id, role_template_id").
		WillReturnRows(sqlmock.NewRows([]string{"organization_id", "user_id", "role_template_id"}))
	env.app.ExpectExec("DELETE FROM organization_member_roles WHERE mirrored_at").
		WillReturnResult(sqlmock.NewResult(0, 0))

	rep, err := Reconcile(context.Background(), env.appDB, env.identityDB)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(rep.DivergentTemplates) != 0 {
		t.Fatalf("DivergentTemplates = %v, want none for a role this build does not define", rep.DivergentTemplates)
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
	if _, err := Reconcile(context.Background(), env.appDB, nil); err == nil {
		t.Fatal("Reconcile succeeded with no identity connection to reconcile from")
	}
	if _, err := Reconcile(context.Background(), nil, env.identityDB); !errors.Is(err, ErrMisrouted) {
		t.Fatalf("Reconcile with no app connection: got %v, want ErrMisrouted", err)
	}
}

// LogReport is the startup line an operator reads, including the divergence
// warning that is the ONLY signal a coupled deployment gets that its roles are
// not this build's. It is exercised here so a nil slice or a missing field
// cannot panic the boot it is supposed to describe.
func TestLogReport_HandlesBothShapes(t *testing.T) {
	LogReport(Report{Templates: "public.role_templates", Assignments: "public.organization_member_roles"})
	LogReport(Report{
		Templates: "public.role_templates", Assignments: "public.organization_member_roles",
		TemplatesCopied: 6, AssignmentsRestated: 12, StaleRemoved: 1,
		DivergentTemplates: []string{"editor", "viewer"},
	})
	if TemplateDivergenceRemedy == "" {
		t.Fatal("the divergence warning carries no remedy, so the only signal a coupled deployment gets says nothing about what to do")
	}
}
