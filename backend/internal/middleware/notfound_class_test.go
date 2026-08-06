// notfound_class_test.go pins the authentication middleware's answer for a
// token naming a user who no longer exists, across terraform-suite-identity's
// store.ErrNotFound change (module v0.24.0).
//
// This is an AUTHORIZATION-shaped instance of the dead-branch defect: the
// middleware read `if err != nil { 500 }` and only then `if user == nil { 401 }`.
// Once a missing user arrives as an error, the 401 is unreachable and a deleted
// user's still-valid JWT gets a 500 — which reads as "the server is broken"
// rather than "this credential is no longer good", and is the wrong signal for
// a client, a load balancer, and an on-call alert alike.
package middleware

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
)

func TestAuthMiddleware_DeletedUser_Returns401(t *testing.T) {
	userRepo, userMock := newUserRepo(t)
	tokenRepo, tokenMock := newTokenRepo(t)
	expectNotRevoked(tokenMock, false)
	// The account behind this otherwise-valid token is gone.
	userMock.ExpectQuery("SELECT id, email, name, oidc_sub").
		WillReturnRows(sqlmock.NewRows(userCols))

	r := authRouter(userRepo, tokenRepo)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+generateTestJWT(t, "deleted-user"))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("token for a deleted user: status = %d, want 401 — a credential "+
			"naming a nonexistent principal is an auth failure, not a server fault (%s)",
			w.Code, w.Body.String())
	}
}

// TestAuthMiddleware_UserLookupFailure_Returns500 is the counterweight: the 401
// above must come from the not-found sentinel specifically, not from a blanket
// "any user-lookup error means unauthenticated". A database fault still 500s,
// so an outage can never be mistaken for a mass credential revocation.
func TestAuthMiddleware_UserLookupFailure_Returns500(t *testing.T) {
	userRepo, userMock := newUserRepo(t)
	tokenRepo, tokenMock := newTokenRepo(t)
	expectNotRevoked(tokenMock, false)
	userMock.ExpectQuery("SELECT id, email, name, oidc_sub").
		WillReturnError(errors.New("connection refused"))

	r := authRouter(userRepo, tokenRepo)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+generateTestJWT(t, "user-1"))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("user lookup failure: status = %d, want 500 (%s)", w.Code, w.Body.String())
	}
}

// TestAuthenticateAPIKey_OrphanedKey_Denies keeps the API-key path failing
// CLOSED. The key's bcrypt hash matched, but its owner is gone: the collapsed
// `uErr != nil || user == nil` check absorbs the new sentinel, and this pins
// that it still denies rather than authenticating an ownerless credential.
func TestAuthenticateAPIKey_OrphanedKey_Denies(t *testing.T) {
	fullKey, hash, prefix := mintTestKey(t)
	keyRepo, keyMock := newAPIKeyRepo(t)
	userRepo, userMock := newUserRepo(t)

	keyMock.ExpectQuery("FROM api_keys").WithArgs(prefix).
		WillReturnRows(keyRow(hash, prefix, `["state:read"]`, nil))
	// Owner row is gone -> ErrNotFound -> must deny, not authenticate.
	userMock.ExpectQuery("SELECT id, email, name, oidc_sub").
		WillReturnRows(sqlmock.NewRows(userCols))

	r := authRouterWithKeys(userRepo, nil, keyRepo)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+fullKey)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("api key whose owner no longer exists: status = %d, want 401 (%s)",
			w.Code, w.Body.String())
	}
}
