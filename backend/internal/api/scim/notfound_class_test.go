// notfound_class_test.go pins SCIM's status codes across
// terraform-suite-identity's store.ErrNotFound change (module v0.24.0).
//
// SCIM is the surface where getting this wrong is most expensive: provisioning
// clients retry 5xx aggressively and treat 404 as terminal, so a miss that
// answers 500 instead of 404 turns one absent user into an unbounded retry
// loop against this API.
package scim

import (
	"net/http"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
)

// TestGetUser_Missing_Returns404 covers the classic dead-branch shape: the
// handler read `if err != nil { 500 }` and only then `if user == nil { 404 }`,
// so once a miss became an error the 404 became unreachable.
func TestGetUser_Missing_Returns404(t *testing.T) {
	r, mock := newSCIM(t)
	mock.ExpectQuery("SELECT id, email, name, oidc_sub").WithArgs("ghost").
		WillReturnRows(sqlmock.NewRows(userCols))

	w := doJSON(r, http.MethodGet, "/scim/v2/Users/ghost", "")
	if w.Code != http.StatusNotFound {
		t.Fatalf("GET missing user: status = %d, want 404 (%s)", w.Code, w.Body.String())
	}
}

func TestGetGroup_Missing_Returns404(t *testing.T) {
	r, mock := newSCIM(t)
	mock.ExpectQuery("FROM organizations").WithArgs("ghost").
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "display_name", "idp_type", "idp_name", "created_at", "updated_at"}))

	w := doJSON(r, http.MethodGet, "/scim/v2/Groups/ghost", "")
	if w.Code != http.StatusNotFound {
		t.Fatalf("GET missing group: status = %d, want 404", w.Code)
	}
}

// TestPutUser_RacedDelete_Returns404 covers the read-then-write race: the user
// was there for the read and gone by the UPDATE. Before v0.24.0 the zero-row
// UPDATE returned nil and the handler answered 200 with a user it had not
// written; now it is distinguishable, and 404 (not 500) is what stops a SCIM
// client from retrying forever.
func TestPutUser_RacedDelete_Returns404(t *testing.T) {
	r, mock := newSCIM(t)
	mock.ExpectQuery("SELECT id, email, name, oidc_sub").WithArgs("u1").
		WillReturnRows(userRow("u1", "a@b.c", "Alice"))
	mock.ExpectExec("UPDATE users").WillReturnResult(sqlmock.NewResult(0, 0))

	w := doJSON(r, http.MethodPut, "/scim/v2/Users/u1", `{"userName":"new@b.c"}`)
	if w.Code != http.StatusNotFound {
		t.Fatalf("PUT raced against a delete: status = %d, want 404 (%s)", w.Code, w.Body.String())
	}
}

func TestPatchUser_RacedDelete_Returns404(t *testing.T) {
	r, mock := newSCIM(t)
	mock.ExpectQuery("SELECT id, email, name, oidc_sub").WithArgs("u1").
		WillReturnRows(userRow("u1", "a@b.c", "Alice"))
	mock.ExpectExec("UPDATE users").WillReturnResult(sqlmock.NewResult(0, 0))

	w := doJSON(r, http.MethodPatch, "/scim/v2/Users/u1",
		`{"Operations":[{"op":"replace","path":"displayName","value":"Alicia"}]}`)
	if w.Code != http.StatusNotFound {
		t.Fatalf("PATCH raced against a delete: status = %d, want 404 (%s)", w.Code, w.Body.String())
	}
}

// TestDeleteUser_NoMembershipsToStrip_Returns204 is the idempotency case that
// matters most for an IdP integration: SCIM DELETE is a SOFT delete (strip
// memberships), and HR systems replay it. RemoveAllMembershipsForUser is a BULK
// sweep, so zero rows is a count of zero rather than ErrNotFound — the endpoint
// stays 204 for a user who is already deprovisioned.
func TestDeleteUser_NoMembershipsToStrip_Returns204(t *testing.T) {
	r, mock := newSCIM(t)
	mock.ExpectQuery("SELECT id, email, name, oidc_sub").WithArgs("u1").
		WillReturnRows(userRow("u1", "a@b.c", "Alice"))
	mock.ExpectQuery("DELETE FROM organization_members").WithArgs("u1").
		WillReturnRows(removedOrgRows(0))

	w := doJSON(r, http.MethodDelete, "/scim/v2/Users/u1", "")
	if w.Code != http.StatusNoContent {
		t.Fatalf("repeat SCIM deactivation: status = %d, want 204 (%s)", w.Code, w.Body.String())
	}
}
