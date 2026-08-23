package api

import (
	"errors"
	"net/http"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// A key is minted into the organization the caller is ACTING in, and only when
// its owner belongs to that organization.
//
// The two halves of this rule used to disagree. #453 made authentication refuse
// a key unless its owner is still a member of the key's organization — while
// mintKey stamped every key with the deployment's DEFAULT organization whoever
// minted it. So a member of the second organization received a key stamped with
// the first, which their own next request then refused. The column was a
// constant, which is also why tenantscope's KeyBindsOrganization is off.

var errProbeMint = errors.New("membership lookup failed")

func TestMintKey_StampsTheActingOrganization(t *testing.T) {
	e := newAPIKeysEnv(t)
	expectOwnerIsMember(e.mock, testActingOrg, "u1")
	// The organization is bound as $3 -- the statement is
	// `SELECT $1, $2, o.id, ... FROM organizations o WHERE o.id = $3`, so $3 is
	// what decides which organization the row lands in. Asserting on that
	// argument is what distinguishes "stamped with the acting organization" from
	// "stamped with whatever the old default lookup returned".
	e.mock.ExpectExec("INSERT INTO api_keys").
		WithArgs(sqlmock.AnyArg(), "u1", testActingOrg, sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := e.do(http.MethodPost, "/api/v1/apikeys", `{"name":"ci","scopes":["state:read"]}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d (%s)", w.Code, w.Body.String())
	}
	if err := e.mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("the key was not stamped with the acting organization: %v", err)
	}
}

// TestMintKey_RefusesAnOwnerWhoIsNotAMember. Creating the key anyway would
// produce one that authentication refuses on its very first use — a credential
// that exists, was shown to its holder once, and never works.
func TestMintKey_RefusesAnOwnerWhoIsNotAMember(t *testing.T) {
	e := newAPIKeysEnv(t)
	e.mock.ExpectQuery("FROM organization_members").WithArgs(testActingOrg, "u1").
		WillReturnRows(sqlmock.NewRows([]string{"organization_id", "user_id", "role_template_id", "created_at"}))
	// No INSERT scripted: the refusal must precede the write.

	w := e.do(http.MethodPost, "/api/v1/apikeys", `{"name":"ci","scopes":["state:read"]}`)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (%s)", w.Code, w.Body.String())
	}
	if err := e.mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("a key was written for a non-member owner: %v", err)
	}
}

// TestMintKey_FailsClosedOnAMembershipLookupError. A failed lookup is not an
// answer, and must not be read as "member".
func TestMintKey_FailsClosedOnAMembershipLookupError(t *testing.T) {
	e := newAPIKeysEnv(t)
	e.mock.ExpectQuery("FROM organization_members").WithArgs(testActingOrg, "u1").
		WillReturnError(errProbeMint)

	w := e.do(http.MethodPost, "/api/v1/apikeys", `{"name":"ci","scopes":["state:read"]}`)
	if w.Code == http.StatusCreated {
		t.Fatalf("a key was minted despite a failed membership lookup (%s)", w.Body.String())
	}
	if err := e.mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
}
