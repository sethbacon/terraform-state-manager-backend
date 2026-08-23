package middleware

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

// "An API key must be tied to a member of an organization, or revoked."
//
// This file covers the authentication half of that rule. Revocation of the rows
// that cannot satisfy it is deliberately NOT here: DELETE is the only revocation
// the schema still has (suite-identity 000004 dropped is_active), the secret is
// shown once so it is irreversible, and identity.api_keys carries no application
// discriminator — so in a deployment sharing one identity store with
// terraform-registry, a sweep from this side cannot tell its own rows from the
// sibling's. Refusing at the boundary is reversible and reaches only what is
// presented to THIS app.

// expectCombinedScopes stubs the live-scope cap that follows the membership
// assertion, so a test can script the WHOLE happy path and leave the guard under
// test as the only thing that can refuse.
func expectCombinedScopes(mock sqlmock.Sqlmock, userID string) {
	mock.ExpectQuery("FROM organization_members om").WithArgs(userID).
		WillReturnRows(sqlmock.NewRows(mwMembershipCols).
			AddRow("org1", "default", nil, time.Now(), "viewer", "Viewer", []byte(`["state:read"]`)))
}

// errProbe stands in for any non-ErrNotFound failure of the membership lookup.
var errProbe = errors.New("membership lookup failed")

func keyRowOwnedBy(hash, prefix, userID, orgID string) *sqlmock.Rows {
	var owner any
	if userID != "" {
		owner = userID
	}
	return sqlmock.NewRows(apiKeyCols).
		AddRow("k1", owner, orgID, "ci", nil, hash, prefix, []byte(`["state:read"]`), nil, nil, nil, time.Now())
}

// probeWithKey mints ONE key and hands its hash and prefix to the row builder.
//
// An earlier version minted a key for the request and let each test mint its own
// for the row. The two never matched, so every bcrypt compare failed and every
// test 401'd — including the two that assert a 401. They passed without
// exercising the guard at all. The negative control below is what exposed that,
// which is the whole reason it is here.
func probeWithKey(t *testing.T, row func(hash, prefix string) *sqlmock.Rows, stub func(userMock, orgMock sqlmock.Sqlmock)) int {
	t.Helper()
	fullKey, hash, prefix := mintTestKey(t)
	keyRepo, keyMock := newAPIKeyRepo(t)
	userRepo, userMock := newUserRepo(t)
	orgRepo, orgMock := newOrgRepoMW(t)

	keyMock.ExpectQuery("FROM api_keys").WithArgs(prefix, sqlmock.AnyArg()).WillReturnRows(row(hash, prefix))
	// Consumed only on the success path (UpdateLastUsed fires in a goroutine
	// after a key authenticates); harmless as an unmet expectation otherwise.
	keyMock.ExpectExec("UPDATE api_keys").WillReturnResult(sqlmock.NewResult(0, 1))
	if stub != nil {
		stub(userMock, orgMock)
	}

	r := gin.New()
	r.Use(AuthMiddleware(userRepo, nil, keyRepo, orgRepo, nil, nil))
	r.GET("/", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{}) })
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+fullKey)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w.Code
}

// TestAPIKeyWithNoOwnerIsRefused is #438. Every check in this path used to be
// gated on `userID != ""` with no else, so an owner-less key skipped the
// owner-exists lookup AND the live-scope cap beneath it — the one credential in
// the system whose privileges tracked nobody, valid at its mint-time scopes
// forever.
//
// EVERY DOWNSTREAM QUERY IS STUBBED TO SUCCEED, which is the point. With them
// unstubbed the request 401s because a lookup errors, and the test passes
// whether or not the guard exists — verified: removing the guard left it green.
// Scripting the rest of the path so it would otherwise reach 200 is what makes
// this assertion about the refusal rather than about a missing fixture.
func TestAPIKeyWithNoOwnerIsRefused(t *testing.T) {
	// A genuine key row, correct hash, unexpired, bound to an organization —
	// everything right except that nobody owns it.
	row := func(hash, prefix string) *sqlmock.Rows { return keyRowOwnedBy(hash, prefix, "", "org1") }
	code := probeWithKey(t, row, func(userMock, orgMock sqlmock.Sqlmock) {
		expectUserFound(userMock, "")
		expectKeyOwnerIsMember(orgMock, "org1", "")
		expectCombinedScopes(orgMock, "")
	})
	if code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401: an owner-less key authenticated", code)
	}
}

// TestAPIKeyWithNoBoundOrganizationIsRefused covers the other half of "tied to a
// member of an org": an owner alone is not a tie. Same fixture discipline — the
// rest of the path is scripted to succeed.
func TestAPIKeyWithNoBoundOrganizationIsRefused(t *testing.T) {
	row := func(hash, prefix string) *sqlmock.Rows { return keyRowOwnedBy(hash, prefix, "u1", "") }
	code := probeWithKey(t, row, func(userMock, orgMock sqlmock.Sqlmock) {
		expectUserFound(userMock, "u1")
		expectKeyOwnerIsMember(orgMock, "", "u1")
		expectCombinedScopes(orgMock, "u1")
	})
	if code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401: a key bound to no organization authenticated", code)
	}
}

// TestAPIKeyWhoseOwnerLeftTheOrganizationIsRefused is the half the scope cap
// cannot do on its own.
//
// GetUserCombinedScopes UNIONS across every organization the owner still belongs
// to. A user removed from organization A but still in B therefore keeps a full
// scope set, so the cap passes and A's key keeps working — against A's data. The
// membership assertion is what sees the removal.
func TestAPIKeyWhoseOwnerLeftTheOrganizationIsRefused(t *testing.T) {
	row := func(hash, prefix string) *sqlmock.Rows { return keyRowOwnedBy(hash, prefix, "u1", "org1") }
	code := probeWithKey(t, row, func(userMock, orgMock sqlmock.Sqlmock) {
		expectUserFound(userMock, "u1")
		// No membership row for (org1, u1): the user was removed from org1.
		orgMock.ExpectQuery("FROM organization_members").WithArgs("org1", "u1").
			WillReturnRows(sqlmock.NewRows([]string{"organization_id", "user_id", "role_template_id", "created_at"}))
	})
	if code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401: a key outlived its owner's membership", code)
	}
}

// TestAPIKeyOwnedByAMemberAuthenticates is the negative control. Without it the
// two refusals above are satisfied by a middleware that refuses everything.
func TestAPIKeyOwnedByAMemberAuthenticates(t *testing.T) {
	row := func(hash, prefix string) *sqlmock.Rows { return keyRowOwnedBy(hash, prefix, "u1", "org1") }
	code := probeWithKey(t, row, func(userMock, orgMock sqlmock.Sqlmock) {
		expectUserFound(userMock, "u1")
		expectKeyOwnerIsMember(orgMock, "org1", "u1")
		orgMock.ExpectQuery("FROM organization_members om").WithArgs("u1").
			WillReturnRows(sqlmock.NewRows(mwMembershipCols).
				AddRow("org1", "default", nil, time.Now(), "viewer", "Viewer", []byte(`["state:read"]`)))
	})
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200: a validly tied key was refused", code)
	}
}

// TestMembershipLookupFailureFailsClosed keeps a failed lookup from being read as
// "not a member" — and, more importantly, from being read as "member". The
// underlying CheckMembership absorbs ErrNotFound into (false, nil) but returns
// every other error, precisely so the two can be told apart here.
func TestMembershipLookupFailureFailsClosed(t *testing.T) {
	row := func(hash, prefix string) *sqlmock.Rows { return keyRowOwnedBy(hash, prefix, "u1", "org1") }
	code := probeWithKey(t, row, func(userMock, orgMock sqlmock.Sqlmock) {
		expectUserFound(userMock, "u1")
		orgMock.ExpectQuery("FROM organization_members").WithArgs("org1", "u1").
			WillReturnError(errProbe)
		// Scripted so that treating the error as "member" would otherwise reach
		// 200. Without this the mutant 401s on the unstubbed scopes read and the
		// test passes for the wrong reason.
		expectCombinedScopes(orgMock, "u1")
	})
	if code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401: a failed membership lookup admitted the key", code)
	}
}
