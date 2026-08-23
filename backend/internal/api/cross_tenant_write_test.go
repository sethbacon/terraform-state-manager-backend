package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/terraform-state-manager/terraform-state-manager/internal/tenantscope"
)

// A caller must not be able to MUTATE a row their organization does not own.
//
// This is the third side of the partition, and it went unexamined. #436 stamped
// every INSERT and the read side is being scoped separately, but every UPDATE
// and DELETE on a partition root found its row BY ID ALONE — no organization
// predicate in the statement, no ownership check in the handler, and no
// middleware.TenantScope on the route to check against.
//
// With one organization that is invisible. With two it means a caller holding
// sources:manage in organization B can delete organization A's source by id, and
// state_sources cascades to eight dependent tables. A read leak discloses; this
// destroys.

const otherTenant = "99999999-9999-4999-8999-999999999999"

// TestDeleteSource_RefusesAnotherOrganizationsRow. The scoped DELETE matches no
// row, so RowsAffected is 0 and the handler must report not-found rather than a
// cheerful 204 for a delete that removed nothing.
func TestDeleteSource_RefusesAnotherOrganizationsRow(t *testing.T) {
	e := newSourcesEnvWithScope(t, tenantscope.Scope{OrgIDs: []string{otherTenant}})
	// The row belongs to testActingOrg; the caller is in otherTenant only.
	e.mock.ExpectExec(`DELETE FROM state_sources[\s\S]*organization_id`).
		WithArgs("s1", []string{otherTenant}).
		WillReturnResult(sqlmock.NewResult(0, 0)) // nothing matched

	w := e.do(http.MethodDelete, "/api/v1/sources/s1", "")
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (%s)", w.Code, w.Body.String())
	}
	// 404 and not 403: a caller must not be able to enumerate ids and learn
	// which of them name real sources in some other organization.
	if body := w.Body.String(); strings.Contains(body, "forbidden") {
		t.Errorf("the refusal discloses that the row exists elsewhere: %s", body)
	}
	if err := e.mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("the DELETE was not scoped to the caller's organization: %v", err)
	}
}

// TestDeleteSource_BindsTheCallersOrganization is the positive control: without
// it, a handler that refused everything would satisfy the test above.
func TestDeleteSource_BindsTheCallersOrganization(t *testing.T) {
	e := newSourcesEnv(t)
	e.mock.ExpectExec(`DELETE FROM state_sources[\s\S]*organization_id`).
		WithArgs("s1", []string{testActingOrg}).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if w := e.do(http.MethodDelete, "/api/v1/sources/s1", ""); w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 (%s)", w.Code, w.Body.String())
	}
	if err := e.mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
}

// TestUpdateSource_RefusesAnotherOrganizationsRow. An update rewrites the
// connector config and, when credentials are supplied, the stored secret — so
// reaching another organization's row here is a credential overwrite, not just
// a rename.
func TestUpdateSource_RefusesAnotherOrganizationsRow(t *testing.T) {
	e := newSourcesEnvWithScope(t, tenantscope.Scope{OrgIDs: []string{otherTenant}})
	// The pre-read is still unscoped (the read side is a separate phase), so the
	// handler loads the row and only the WRITE refuses. That is exactly the case
	// worth pinning: the row is visible and must still be unwritable.
	e.expectSource("s1", e.dir)
	e.mock.ExpectQuery(`UPDATE state_sources SET[\s\S]*organization_id`).
		WithArgs("s1", "renamed", nil, sqlmock.AnyArg(), sqlmock.AnyArg(), nil, []string{otherTenant}).
		WillReturnRows(sqlmock.NewRows(apiSourceCols)) // no row matched

	body := `{"name":"renamed","config":{"base_path":"` + e.dir + `"}}`
	w := e.do(http.MethodPut, "/api/v1/sources/s1", body)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (%s)", w.Code, w.Body.String())
	}
	if err := e.mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("the UPDATE was not scoped to the caller's organization: %v", err)
	}
}

// TestMutatingSourceRoutes_RefuseAnUnresolvedScope. If middleware.TenantScope
// came unwired on these routes, treating the absence as "no scope, carry on"
// would silently restore the unscoped statement. It is a wiring fault, not an
// empty scope.
func TestMutatingSourceRoutes_RefuseAnUnresolvedScope(t *testing.T) {
	for _, tc := range []struct {
		name, method, path, body string
	}{
		{"delete", http.MethodDelete, "/api/v1/sources/s1", ""},
		{"update", http.MethodPut, "/api/v1/sources/s1", `{"name":"x"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newSourcesEnvWithoutScope(t)
			// No statements scripted: reaching the database at all is the failure.
			w := e.do(tc.method, tc.path, tc.body)
			if w.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want 500 (%s)", w.Code, w.Body.String())
			}
			if err := e.mock.ExpectationsWereMet(); err != nil {
				t.Fatalf("a statement ran with no tenant scope resolved: %v", err)
			}
		})
	}
}
