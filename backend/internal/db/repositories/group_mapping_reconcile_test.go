package repositories

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
)

// Reconcile tests that need no database, pinning the DECISIONS the boot
// backfill makes: what it rewrites, what it leaves alone, and what it refuses
// outright. The Postgres file proves the topology properties; this file is
// what gates merges.

// newReconcileMocks builds the two scripted connections the reconcile takes:
// source (where sso_settings resolves) and app (where migration 000036 ran).
func newReconcileMocks(t *testing.T) (sourceMock, appMock sqlmock.Sqlmock, run func() (GroupMappingReconcileReport, error)) {
	t.Helper()
	sourceDB, sMock := newMock(t)
	appDB, aMock := newMock(t)
	return sMock, aMock, func() (GroupMappingReconcileReport, error) {
		return ReconcileGroupMappings(context.Background(), sourceDB, appDB)
	}
}

// expectGroupMappingSourceVerified queues verifyGroupMappingSource's probe --
// the one query the reconcile sends the source side before reading it.
func expectGroupMappingSourceVerified(mock sqlmock.Sqlmock) {
	mock.ExpectQuery("to_regclass").
		WillReturnRows(sqlmock.NewRows([]string{"qualified"}).AddRow("public.sso_settings"))
}

// expectAppTemplateNames queues appRoleTemplateNames -- which reads through
// approles.Store.ListTemplates, the one role_templates funnel, so the shape
// here is that query's. pairs are id, name, ...
func expectAppTemplateNames(mock sqlmock.Sqlmock, pairs ...string) {
	rows := sqlmock.NewRows([]string{"id", "name", "display_name", "description", "scopes", "is_system", "created_at", "updated_at"})
	for i := 0; i+1 < len(pairs); i += 2 {
		rows.AddRow(pairs[i], pairs[i+1], pairs[i+1], nil, []byte(`[]`), false, time.Now(), time.Now())
	}
	mock.ExpectQuery("SELECT id, name, COALESCE").WillReturnRows(rows)
}

// expectStoredOverlay queues readStoredGroupMappings with the given
// oidc_group_mappings JSON. Pass no argument for "no overlay row".
func expectStoredOverlay(mock sqlmock.Sqlmock, mappingsJSON ...string) {
	rows := sqlmock.NewRows([]string{"oidc_group_mappings"})
	for _, m := range mappingsJSON {
		rows.AddRow([]byte(m))
	}
	mock.ExpectQuery("SELECT oidc_group_mappings FROM sso_settings").WillReturnRows(rows)
}

// expectMirroredGroupMappings queues readMirroredGroupMappings. Each row is
// position, group, organization, role name, role id or nil.
func expectMirroredGroupMappings(mock sqlmock.Sqlmock, rows ...[5]interface{}) {
	r := sqlmock.NewRows([]string{"position", "group_name", "organization_name", "role_template_name", "role_template_id"})
	for _, row := range rows {
		r.AddRow(row[0], row[1], row[2], row[3], row[4])
	}
	mock.ExpectQuery("SELECT position, group_name").WillReturnRows(r)
}

const gmOneMapping = `[{"group":"eng","organization":"alpha","role":"editor"}]`

// TestReconcileGroupMappings_BackfillsAnEmptyMirror is the upgrade boot: the
// overlay has one mapping whose role resolves, the mirror is empty, and the
// reconcile writes exactly those rows -- resolving the role name to TSM's own
// template id inside the insert.
func TestReconcileGroupMappings_BackfillsAnEmptyMirror(t *testing.T) {
	sourceMock, appMock, run := newReconcileMocks(t)

	expectGroupMappingMirrorVerified(appMock)
	expectGroupMappingSourceVerified(sourceMock)
	expectAppTemplateNames(appMock, gmRoleID, "editor")
	expectStoredOverlay(sourceMock, gmOneMapping)
	expectMirroredGroupMappings(appMock)

	// Replace performs its own name resolution through the same funnel, so a
	// rewrite reads the templates a second time.
	expectAppTemplateNames(appMock, gmRoleID, "editor")
	appMock.ExpectBegin()
	appMock.ExpectExec("DELETE FROM group_mappings").WillReturnResult(sqlmock.NewResult(0, 0))
	appMock.ExpectExec("INSERT INTO group_mappings").
		WithArgs(0, "eng", "alpha", "editor", gmRoleID).
		WillReturnResult(sqlmock.NewResult(0, 1))
	appMock.ExpectCommit()

	report, err := run()
	if err != nil {
		t.Fatalf("ReconcileGroupMappings: %v", err)
	}
	if !report.OverlayPresent || report.SourceMappings != 1 || !report.Rewritten ||
		report.UnresolvedRoleRefs != 0 || report.OverlayUnparseable {
		t.Fatalf("report: %+v", report)
	}
	if err := appMock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
	// Exercise the boot log rendering while a populated report is in hand.
	_ = report.LogValue()
}

// TestReconcileGroupMappings_SteadyStateWritesNothing pins the property that
// makes an every-boot reconcile affordable: when the mirror already equals the
// overlay -- fields, order AND role resolution -- no write statement is issued
// at all. sqlmock fails on any unexpected statement, so the assertion is the
// absence of expectations beyond the reads.
func TestReconcileGroupMappings_SteadyStateWritesNothing(t *testing.T) {
	sourceMock, appMock, run := newReconcileMocks(t)

	expectGroupMappingMirrorVerified(appMock)
	expectGroupMappingSourceVerified(sourceMock)
	expectAppTemplateNames(appMock, gmRoleID, "editor")
	expectStoredOverlay(sourceMock, gmOneMapping)
	expectMirroredGroupMappings(appMock,
		[5]interface{}{0, "eng", "alpha", "editor", gmRoleID})

	report, err := run()
	if err != nil {
		t.Fatalf("ReconcileGroupMappings: %v", err)
	}
	if report.Rewritten {
		t.Fatalf("steady state wrote something: %+v", report)
	}
	if err := appMock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// TestReconcileGroupMappings_RewritesAStaleRoleResolution pins that equality
// includes the DERIVED column: same fields, same order, but a role_template_id
// that no longer matches what the name resolves to forces the rewrite.
func TestReconcileGroupMappings_RewritesAStaleRoleResolution(t *testing.T) {
	sourceMock, appMock, run := newReconcileMocks(t)
	const otherID = "cccccccc-0000-4000-8000-000000000012"

	expectGroupMappingMirrorVerified(appMock)
	expectGroupMappingSourceVerified(sourceMock)
	expectAppTemplateNames(appMock, otherID, "editor")
	expectStoredOverlay(sourceMock, gmOneMapping)
	expectMirroredGroupMappings(appMock,
		[5]interface{}{0, "eng", "alpha", "editor", gmRoleID})

	expectAppTemplateNames(appMock, otherID, "editor")
	appMock.ExpectBegin()
	appMock.ExpectExec("DELETE FROM group_mappings").WillReturnResult(sqlmock.NewResult(0, 1))
	appMock.ExpectExec("INSERT INTO group_mappings").WillReturnResult(sqlmock.NewResult(0, 1))
	appMock.ExpectCommit()

	report, err := run()
	if err != nil {
		t.Fatalf("ReconcileGroupMappings: %v", err)
	}
	if !report.Rewritten {
		t.Fatalf("stale resolution not rewritten: %+v", report)
	}
}

// TestReconcileGroupMappings_ClearsWhenTheOverlayIsGone pins the direction
// that would otherwise GRANT after the cutover: mirror rows whose overlay no
// longer exists (or no longer lists them) are deleted.
func TestReconcileGroupMappings_ClearsWhenTheOverlayIsGone(t *testing.T) {
	sourceMock, appMock, run := newReconcileMocks(t)

	expectGroupMappingMirrorVerified(appMock)
	expectGroupMappingSourceVerified(sourceMock)
	expectAppTemplateNames(appMock)
	expectStoredOverlay(sourceMock) // no overlay row at all
	expectMirroredGroupMappings(appMock,
		[5]interface{}{0, "eng", "alpha", "editor", nil})

	appMock.ExpectBegin()
	appMock.ExpectExec("DELETE FROM group_mappings").WillReturnResult(sqlmock.NewResult(0, 1))
	appMock.ExpectCommit()

	report, err := run()
	if err != nil {
		t.Fatalf("ReconcileGroupMappings: %v", err)
	}
	if report.OverlayPresent || !report.Rewritten {
		t.Fatalf("orphaned rows not cleared: %+v", report)
	}
	if err := appMock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// TestReconcileGroupMappings_CountsWhatItCannotResolve pins the two per-row
// oddities: a role name with no template is mirrored with a NULL id and
// counted, and an oidc_group_mappings value that does not decode is counted
// while its effective mapping list -- none, exactly as the overlay read path
// treats it -- is mirrored faithfully as no rows.
func TestReconcileGroupMappings_CountsWhatItCannotResolve(t *testing.T) {
	sourceMock, appMock, run := newReconcileMocks(t)

	expectGroupMappingMirrorVerified(appMock)
	expectGroupMappingSourceVerified(sourceMock)
	expectAppTemplateNames(appMock)
	expectStoredOverlay(sourceMock, gmOneMapping)
	expectMirroredGroupMappings(appMock)

	expectAppTemplateNames(appMock)
	appMock.ExpectBegin()
	appMock.ExpectExec("DELETE FROM group_mappings").WillReturnResult(sqlmock.NewResult(0, 0))
	appMock.ExpectExec("INSERT INTO group_mappings").WillReturnResult(sqlmock.NewResult(0, 1))
	appMock.ExpectCommit()

	report, err := run()
	if err != nil {
		t.Fatalf("ReconcileGroupMappings: %v", err)
	}
	if report.UnresolvedRoleRefs != 1 {
		t.Fatalf("unresolved role not counted: %+v", report)
	}

	sourceMock2, appMock2, run2 := newReconcileMocks(t)
	expectGroupMappingMirrorVerified(appMock2)
	expectGroupMappingSourceVerified(sourceMock2)
	expectAppTemplateNames(appMock2)
	expectStoredOverlay(sourceMock2, `"not a list"`)
	expectMirroredGroupMappings(appMock2)

	report2, err := run2()
	if err != nil {
		t.Fatalf("ReconcileGroupMappings: %v", err)
	}
	if !report2.OverlayUnparseable || report2.Rewritten || report2.SourceMappings != 0 {
		t.Fatalf("unparseable overlay mishandled: %+v", report2)
	}
}

// TestReconcileGroupMappings_RefusesStructuralFailures pins each early exit:
// a mirror that does not resolve, a misrouted app connection, a source that
// does not resolve, and each read or write that fails must surface as an error
// naming the step -- never as a quiet, empty reconcile.
func TestReconcileGroupMappings_RefusesStructuralFailures(t *testing.T) {
	t.Run("mirror unreachable", func(t *testing.T) {
		_, appMock, run := newReconcileMocks(t)
		appMock.ExpectQuery("to_regclass").
			WithArgs("organization_members").
			WillReturnRows(sqlmock.NewRows([]string{"qualified"}).AddRow(nil))
		appMock.ExpectQuery("to_regclass").
			WithArgs("group_mappings").
			WillReturnRows(sqlmock.NewRows([]string{"qualified"}).AddRow(nil))
		_, err := run()
		if err == nil || !strings.Contains(err.Error(), "group-mapping table unusable") {
			t.Fatalf("want the mirror refusal, got %v", err)
		}
	})
	t.Run("app connection misrouted", func(t *testing.T) {
		_, appMock, run := newReconcileMocks(t)
		appMock.ExpectQuery("to_regclass").
			WithArgs("organization_members").
			WillReturnRows(sqlmock.NewRows([]string{"qualified"}).AddRow("identity.organization_members"))
		if _, err := run(); !errors.Is(err, ErrGroupMappingMirrorMisrouted) {
			t.Fatalf("want the misrouting refusal, got %v", err)
		}
	})
	t.Run("source unreachable", func(t *testing.T) {
		sourceMock, appMock, run := newReconcileMocks(t)
		expectGroupMappingMirrorVerified(appMock)
		sourceMock.ExpectQuery("to_regclass").
			WillReturnRows(sqlmock.NewRows([]string{"qualified"}).AddRow(nil))
		_, err := run()
		if err == nil || !strings.Contains(err.Error(), "refusing to reconcile") {
			t.Fatalf("want the source refusal, got %v", err)
		}
	})
	t.Run("source probe fails", func(t *testing.T) {
		sourceMock, appMock, run := newReconcileMocks(t)
		expectGroupMappingMirrorVerified(appMock)
		sourceMock.ExpectQuery("to_regclass").WillReturnError(errors.New("boom"))
		if _, err := run(); err == nil {
			t.Fatal("want error")
		}
	})
	t.Run("template names read fails", func(t *testing.T) {
		sourceMock, appMock, run := newReconcileMocks(t)
		expectGroupMappingMirrorVerified(appMock)
		expectGroupMappingSourceVerified(sourceMock)
		appMock.ExpectQuery("SELECT id, name, COALESCE").WillReturnError(errors.New("boom"))
		if _, err := run(); err == nil {
			t.Fatal("want error")
		}
	})
	t.Run("source read fails", func(t *testing.T) {
		sourceMock, appMock, run := newReconcileMocks(t)
		expectGroupMappingMirrorVerified(appMock)
		expectGroupMappingSourceVerified(sourceMock)
		expectAppTemplateNames(appMock)
		sourceMock.ExpectQuery("SELECT oidc_group_mappings FROM sso_settings").WillReturnError(errors.New("boom"))
		if _, err := run(); err == nil {
			t.Fatal("want error")
		}
	})
	t.Run("mirror read fails", func(t *testing.T) {
		sourceMock, appMock, run := newReconcileMocks(t)
		expectGroupMappingMirrorVerified(appMock)
		expectGroupMappingSourceVerified(sourceMock)
		expectAppTemplateNames(appMock)
		expectStoredOverlay(sourceMock)
		appMock.ExpectQuery("SELECT position, group_name").WillReturnError(errors.New("boom"))
		if _, err := run(); err == nil {
			t.Fatal("want error")
		}
	})
	t.Run("rewrite fails", func(t *testing.T) {
		sourceMock, appMock, run := newReconcileMocks(t)
		expectGroupMappingMirrorVerified(appMock)
		expectGroupMappingSourceVerified(sourceMock)
		expectAppTemplateNames(appMock)
		expectStoredOverlay(sourceMock, gmOneMapping)
		expectMirroredGroupMappings(appMock)
		expectAppTemplateNames(appMock)
		appMock.ExpectBegin().WillReturnError(errors.New("boom"))
		if _, err := run(); err == nil {
			t.Fatal("want error")
		}
	})
}

// TestSameGroupMappingList pins the equality the steady-state decision hangs
// on, one differing field per case.
func TestSameGroupMappingList(t *testing.T) {
	id := gmRoleID
	base := func() []mirroredGroupMapping {
		return []mirroredGroupMapping{{Position: 0, Group: "g", Organization: "o", RoleName: "r", RoleTemplateID: &id}}
	}
	if !sameGroupMappingList(base(), base()) {
		t.Fatal("identical lists compared unequal")
	}
	cases := map[string]func([]mirroredGroupMapping) []mirroredGroupMapping{
		"length":       func(l []mirroredGroupMapping) []mirroredGroupMapping { return l[:0] },
		"position":     func(l []mirroredGroupMapping) []mirroredGroupMapping { l[0].Position = 1; return l },
		"group":        func(l []mirroredGroupMapping) []mirroredGroupMapping { l[0].Group = "x"; return l },
		"organization": func(l []mirroredGroupMapping) []mirroredGroupMapping { l[0].Organization = "x"; return l },
		"role name":    func(l []mirroredGroupMapping) []mirroredGroupMapping { l[0].RoleName = "x"; return l },
		"role id":      func(l []mirroredGroupMapping) []mirroredGroupMapping { l[0].RoleTemplateID = nil; return l },
	}
	for name, mutate := range cases {
		if sameGroupMappingList(mutate(base()), base()) {
			t.Errorf("lists differing in %s compared equal", name)
		}
	}
}
