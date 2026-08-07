package api

import (
	"net/http"
	"strings"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
)

// auditRowCols mirrors ListAuditLogs' projection. actor_email is COLUMN 10,
// between created_at and the joined user_email/user_name, as of identity
// v0.25.0's migration 000007: it is the actor's address as it stood when the
// entry was written, STORED on the row so attribution survives the users row
// being deleted, while user_email/user_name stay transient join fields that go
// nil once the user is gone.
var auditRowCols = []string{
	"id", "user_id", "organization_id", "action", "resource_type", "resource_id",
	"metadata", "ip_address", "created_at", "actor_email", "user_email", "user_name",
}

func auditRow(rows *sqlmock.Rows, id, action, email string) *sqlmock.Rows {
	return rows.AddRow(id, "u1", nil, action, "user", nil, nil, "10.0.0.9",
		time.Date(2026, 7, 10, 8, 0, 0, 0, time.UTC), email, email, "Alice")
}

// expectAuditPage queues one ListAuditLogs round-trip: the COUNT then the page.
func expectAuditPage(mock sqlmock.Sqlmock, total int, page *sqlmock.Rows) {
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM audit_logs`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(total))
	mock.ExpectQuery("SELECT al.id, .+ FROM audit_logs").WillReturnRows(page)
}

// auditInsertReturn is what CreateAuditLog's INSERT hands back.
//
// It is a QUERY, not an Exec, as of identity v0.25.0: the statement ends
// `RETURNING actor_email` because it fills that column from the users table with
// a COALESCE subquery when the caller leaves it nil, and returns what it stored.
// An ExpectExec queued against it no longer matches, which is why every audit
// expectation in this package moved to ExpectQuery.
func auditInsertReturn() *sqlmock.Rows {
	return sqlmock.NewRows([]string{"actor_email"}).AddRow(nil)
}

// expectExportSelfAudit queues the audit.export entry the handler writes about itself.
func expectExportSelfAudit(mock sqlmock.Sqlmock) {
	mock.ExpectQuery("INSERT INTO audit_logs").WillReturnRows(auditInsertReturn())
}

func TestExportAuditLogs_CSVPagesThroughAll(t *testing.T) {
	h, mock := newAdminHandlers(t)
	prev := exportAuditPageSize
	exportAuditPageSize = 2
	t.Cleanup(func() { exportAuditPageSize = prev })

	// Three matching rows across two repo pages: the export must not stop at
	// the first page (the old client-side export silently capped at one).
	expectAuditPage(mock, 3, auditRow(auditRow(sqlmock.NewRows(auditRowCols), "a1", "auth.login", "a@x.io"), "a2", "auth.logout", "a@x.io"))
	expectAuditPage(mock, 3, auditRow(sqlmock.NewRows(auditRowCols), "a3", "state.edit", "b@x.io"))
	expectExportSelfAudit(mock)

	w := serveAdmin(h.ExportAuditLogs(), "/x?format=csv")
	if w.Code != http.StatusOK {
		t.Fatalf("export: status = %d (%s)", w.Code, w.Body.String())
	}
	if cd := w.Header().Get("Content-Disposition"); !strings.Contains(cd, `attachment; filename="audit-logs.csv"`) {
		t.Errorf("Content-Disposition = %q", cd)
	}
	body := w.Body.String()
	if !strings.HasPrefix(body, "created_at,action,resource_type,resource_id,user_email,user_name,ip_address") {
		t.Errorf("csv header missing: %s", body)
	}
	for _, want := range []string{"auth.login", "auth.logout", "state.edit", "a@x.io", "b@x.io", "10.0.0.9"} {
		if !strings.Contains(body, want) {
			t.Errorf("csv missing %s: %s", want, body)
		}
	}
	if got := strings.Count(strings.TrimSpace(body), "\n"); got != 3 {
		t.Errorf("csv line breaks = %d, want 3 (header + 3 rows)", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expected two repo pages: %v", err)
	}
}

func TestExportAuditLogs_JSON(t *testing.T) {
	h, mock := newAdminHandlers(t)
	expectAuditPage(mock, 1, auditRow(sqlmock.NewRows(auditRowCols), "a1", "auth.login", "a@x.io"))
	expectExportSelfAudit(mock)

	w := serveAdmin(h.ExportAuditLogs(), "/x?format=json")
	if w.Code != http.StatusOK {
		t.Fatalf("json export: status = %d (%s)", w.Code, w.Body.String())
	}
	if cd := w.Header().Get("Content-Disposition"); !strings.Contains(cd, `filename="audit-logs.json"`) {
		t.Errorf("Content-Disposition = %q", cd)
	}
	for _, want := range []string{`"logs"`, `"auth.login"`, `"a@x.io"`, `"truncated":false`} {
		if !strings.Contains(w.Body.String(), want) {
			t.Errorf("json missing %s: %s", want, w.Body.String())
		}
	}
}

func TestExportAuditLogs_TruncatesAtCap(t *testing.T) {
	h, mock := newAdminHandlers(t)
	prevPage, prevMax := exportAuditPageSize, exportAuditMaxRows
	exportAuditPageSize, exportAuditMaxRows = 2, 2
	t.Cleanup(func() { exportAuditPageSize, exportAuditMaxRows = prevPage, prevMax })

	// Total 3 but the cap stops collection after the first page of 2.
	expectAuditPage(mock, 3, auditRow(auditRow(sqlmock.NewRows(auditRowCols), "a1", "auth.login", "a@x.io"), "a2", "auth.logout", "a@x.io"))
	expectExportSelfAudit(mock)

	w := serveAdmin(h.ExportAuditLogs(), "/x?format=csv")
	if w.Code != http.StatusOK {
		t.Fatalf("capped export: status = %d (%s)", w.Code, w.Body.String())
	}
	if w.Header().Get("X-Truncated") != "true" {
		t.Errorf("X-Truncated header missing on capped export")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("capped export must stop after one page: %v", err)
	}
}

func TestExportAuditLogs_BadFormat(t *testing.T) {
	h, _ := newAdminHandlers(t)
	if w := serveAdmin(h.ExportAuditLogs(), "/x?format=xml"); w.Code != http.StatusBadRequest {
		t.Errorf("bad format: status = %d, want 400", w.Code)
	}
}

func TestExportAuditLogs_FiltersApplied(t *testing.T) {
	h, mock := newAdminHandlers(t)

	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM audit_logs`).WithArgs("auth.login").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	mock.ExpectQuery("SELECT al.id, .+ FROM audit_logs").WithArgs("auth.login", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnRows(auditRow(sqlmock.NewRows(auditRowCols), "a1", "auth.login", "a@x.io"))
	expectExportSelfAudit(mock)

	w := serveAdmin(h.ExportAuditLogs(), "/x?format=csv&action=auth.login")
	if w.Code != http.StatusOK {
		t.Fatalf("filtered export: status = %d (%s)", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("action filter must reach the repository: %v", err)
	}
}
