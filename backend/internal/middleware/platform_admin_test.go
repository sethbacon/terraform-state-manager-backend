package middleware

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"

	"github.com/terraform-state-manager/terraform-state-manager/internal/platformadmin"
)

// The two halves of the elevation contract, on the two principal kinds it keeps
// apart.
//
// A SESSION is resolved against the carrier on every request. AN API KEY IS
// NEVER RESOLVED AGAINST IT — authenticateAPIKey has no carrier parameter at
// all, so the property is structural; what these tests add is that the stored
// `admin` a key may nonetheless be carrying is STRIPPED, which is a line of code
// and therefore a line that can be deleted.

// carrierService builds a platform-admin service over two sqlmock handles: the
// app handle carries the carrier and its outbox, the identity handle the
// resolver and the audit destination. Constructing it performs no I/O, so the
// only queries these tests have to script are the ones a request actually makes.
func carrierService(t *testing.T) (*platformadmin.Service, sqlmock.Sqlmock) {
	t.Helper()
	appDB, appMock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New (app): %v", err)
	}
	t.Cleanup(func() { appDB.Close() })
	identityDB, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New (identity): %v", err)
	}
	t.Cleanup(func() { identityDB.Close() })

	svc, err := platformadmin.New(appDB, identityDB)
	if err != nil {
		t.Fatalf("platformadmin.New: %v", err)
	}
	return svc, appMock
}

// expectCarrierLookup scripts one carrier read answering isAdmin.
func expectCarrierLookup(mock sqlmock.Sqlmock, userID string, isAdmin bool) {
	mock.ExpectQuery(`FROM "platform_admins" WHERE user_id`).WithArgs(userID).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(isAdmin))
}

// scopeProbeRouter reports the effective scope set the middleware published.
func scopeProbeRouter(handler gin.HandlerFunc) *gin.Engine {
	r := gin.New()
	r.Use(handler)
	r.GET("/", func(c *gin.Context) {
		sc, _ := c.Get("scopes")
		c.JSON(http.StatusOK, gin.H{"scopes": sc})
	})
	return r
}

func scopesFromBody(t *testing.T, body string) []string {
	t.Helper()
	var parsed struct {
		Scopes []string `json:"scopes"`
	}
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("decode scopes from %q: %v", body, err)
	}
	return parsed.Scopes
}

func hasScope(scopes []string, want string) bool {
	for _, s := range scopes {
		if s == want {
			return true
		}
	}
	return false
}

// TestSessionScopesElevateFromTheCarrier is the whole point of the session path:
// a token that was minted with NO admin claim carries `admin` on this request
// because the carrier says so right now.
//
// It is also what makes revocation immediate. If elevation came from the token,
// removing the carrier row would take effect whenever that token happened to
// expire — which for TSM's 24h sessions is up to a day after the operator
// believes the privilege is gone.
func TestSessionScopesElevateFromTheCarrier(t *testing.T) {
	userRepo, userMock := newUserRepo(t)
	svc, carrierMock := carrierService(t)
	expectUserFound(userMock, "u1")
	expectCarrierLookup(carrierMock, "u1", true)

	r := scopeProbeRouter(AuthMiddleware(userRepo, nil, nil, nil, nil, svc))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+generateTestJWT(t, "u1"))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	scopes := scopesFromBody(t, w.Body.String())
	if !hasScope(scopes, "admin") {
		t.Errorf("scopes = %v, want admin present: the token carried none and the carrier holds a row for u1", scopes)
	}
	if !hasScope(scopes, "state:read") {
		t.Errorf("scopes = %v, want the token's own state:read retained", scopes)
	}
	if err := carrierMock.ExpectationsWereMet(); err != nil {
		t.Errorf("carrier was not consulted: %v", err)
	}
}

// TestSessionScopesWithoutACarrierRowGrantNoAdmin is the other direction, and
// the one a broken elevation would fail silently: a user with no carrier row and
// no admin in their token must not acquire it.
func TestSessionScopesWithoutACarrierRowGrantNoAdmin(t *testing.T) {
	userRepo, userMock := newUserRepo(t)
	svc, carrierMock := carrierService(t)
	expectUserFound(userMock, "u1")
	expectCarrierLookup(carrierMock, "u1", false)

	r := scopeProbeRouter(AuthMiddleware(userRepo, nil, nil, nil, nil, svc))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+generateTestJWT(t, "u1"))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	scopes := scopesFromBody(t, w.Body.String())
	if hasScope(scopes, "admin") {
		t.Errorf("scopes = %v, want no admin: neither the token nor the carrier confers it", scopes)
	}
}

// TestSessionScopesResolveOnEveryRequest holds the no-cache rule shut.
//
// A memoised carrier answer would reintroduce exactly the window a token claim
// has: the revocation would take effect one TTL later. Two requests, two reads.
func TestSessionScopesResolveOnEveryRequest(t *testing.T) {
	userRepo, userMock := newUserRepo(t)
	svc, carrierMock := carrierService(t)
	token := generateTestJWT(t, "u1")

	expectUserFound(userMock, "u1")
	expectCarrierLookup(carrierMock, "u1", true)
	expectUserFound(userMock, "u1")
	// The grant was revoked between the two requests.
	expectCarrierLookup(carrierMock, "u1", false)

	r := scopeProbeRouter(AuthMiddleware(userRepo, nil, nil, nil, nil, svc))
	call := func() []string {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (%s)", w.Code, w.Body.String())
		}
		return scopesFromBody(t, w.Body.String())
	}

	if first := call(); !hasScope(first, "admin") {
		t.Fatalf("first request scopes = %v, want admin", first)
	}
	if second := call(); hasScope(second, "admin") {
		t.Errorf("second request scopes = %v, want admin gone: the same session must lose it "+
			"the moment the carrier row does, not when the token expires", second)
	}
	if err := carrierMock.ExpectationsWereMet(); err != nil {
		t.Errorf("the carrier was not read once per request: %v", err)
	}
}

// TestSessionCarrierFailureIsAServerFault: an authority question that did not
// resolve is not a completed "no".
//
// 500 rather than 403 deliberately. A denial would tell a platform administrator
// they lack permission during exactly the incident in which they need the admin
// surface, and would look identical to a correct refusal in the logs.
func TestSessionCarrierFailureIsAServerFault(t *testing.T) {
	userRepo, userMock := newUserRepo(t)
	svc, carrierMock := carrierService(t)
	expectUserFound(userMock, "u1")
	carrierMock.ExpectQuery(`FROM "platform_admins" WHERE user_id`).
		WillReturnError(errors.New("connection reset"))

	r := scopeProbeRouter(AuthMiddleware(userRepo, nil, nil, nil, nil, svc))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+generateTestJWT(t, "u1"))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 when the carrier cannot be read (%s)", w.Code, w.Body.String())
	}
}

// TestUnwiredCarrierLeavesTheSessionUnchanged: a nil service is the nil-DB rig
// and a deployment without a carrier. It must not fail requests, and it must not
// confer anything — which it cannot, because in this phase the carrier only ever
// adds.
func TestUnwiredCarrierLeavesTheSessionUnchanged(t *testing.T) {
	userRepo, userMock := newUserRepo(t)
	expectUserFound(userMock, "u1")

	r := scopeProbeRouter(AuthMiddleware(userRepo, nil, nil, nil, nil, nil))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+generateTestJWT(t, "u1"))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	scopes := scopesFromBody(t, w.Body.String())
	if hasScope(scopes, "admin") {
		t.Errorf("scopes = %v: an unwired carrier must confer nothing", scopes)
	}
	if !hasScope(scopes, "state:read") {
		t.Errorf("scopes = %v, want the token's own scopes intact", scopes)
	}
}

// TestAPIKeyNeverCarriesAdminEvenWhenItsOwnerDoes is the mutation-sensitive one.
//
// The existing cap test (TestAuthMiddleware_APIKeyScopesCappedByLiveOwnerScopes)
// removes admin because the OWNER lost it. This one gives the owner admin, so
// grantedSubset keeps the key's stored admin, and the ONLY thing that can remove
// it is platformadmin.KeyScopes. Delete that line and this test fails; the cap
// test does not.
//
// Why it matters: a key is a long-lived bearer credential, frequently unattended
// in CI. It bypasses the cookie CSRF check, is not bound by the session TTL, and
// may be minted with no expiry. `admin` is already excluded from
// assignableKeyScopes (#252), but a key's stored scope set is not a live
// authority statement about anybody — an older role model or a hand-written
// INSERT can put it there — so the auth path strips rather than trusting the
// mint path to have been the only writer.
func TestAPIKeyNeverCarriesAdminEvenWhenItsOwnerDoes(t *testing.T) {
	fullKey, hash, prefix := mintTestKey(t)
	keyRepo, keyMock := newAPIKeyRepo(t)
	userRepo, userMock := newUserRepo(t)
	orgRepo, orgMock := newOrgRepoMW(t)

	keyMock.ExpectQuery("FROM api_keys").WithArgs(prefix, sqlmock.AnyArg()).
		WillReturnRows(keyRow(hash, prefix, `["admin","state:read"]`, nil))
	keyMock.ExpectExec("UPDATE api_keys").WillReturnResult(sqlmock.NewResult(0, 1))
	expectUserFound(userMock, "u1")
	expectKeyOwnerIsMember(orgMock, "org1", "u1")
	// The owner IS an admin, so the live-scope cap keeps every stored scope.
	orgMock.ExpectQuery("FROM organization_members om").WithArgs("u1").
		WillReturnRows(sqlmock.NewRows(mwMembershipCols).
			AddRow("o1", "default", nil, time.Now(), "admin", "Administrator", []byte(`["admin"]`)))

	// A carrier IS wired, and the owner holds a row in it. The API-key path must
	// still not reach it: authenticateAPIKey takes no carrier, so the row below
	// is scripted only to prove no query consumes it.
	svc, carrierMock := carrierService(t)

	r := scopeProbeRouter(AuthMiddleware(userRepo, nil, keyRepo, orgRepo, nil, svc))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+fullKey)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	scopes := scopesFromBody(t, w.Body.String())
	if hasScope(scopes, "admin") {
		t.Errorf("scopes = %v: an API key must never carry admin, even when its owner holds it "+
			"and the key's stored scopes name it", scopes)
	}
	if !hasScope(scopes, "state:read") {
		t.Errorf("scopes = %v: stripping admin must not disturb the key's other scopes", scopes)
	}
	// Nothing was queued on the carrier and nothing may have been asked of it.
	if err := carrierMock.ExpectationsWereMet(); err != nil {
		t.Errorf("the API-key path consulted the platform-admin carrier: %v", err)
	}
}

// TestOptionalAuthElevatesWithoutEverFailing: the logout path publishes the same
// scope set as AuthMiddleware, and a carrier that cannot answer leaves the
// session unelevated rather than failing a request this middleware promises
// never to fail.
func TestOptionalAuthElevatesWithoutEverFailing(t *testing.T) {
	t.Run("elevates", func(t *testing.T) {
		userRepo, userMock := newUserRepo(t)
		svc, carrierMock := carrierService(t)
		expectUserFound(userMock, "u1")
		expectCarrierLookup(carrierMock, "u1", true)

		r := scopeProbeRouter(OptionalAuthMiddleware(userRepo, nil, nil, svc))
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("Authorization", "Bearer "+generateTestJWT(t, "u1"))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if got := scopesFromBody(t, w.Body.String()); !hasScope(got, "admin") {
			t.Errorf("scopes = %v, want admin", got)
		}
	})

	t.Run("carrier failure does not fail the request", func(t *testing.T) {
		userRepo, userMock := newUserRepo(t)
		svc, carrierMock := carrierService(t)
		expectUserFound(userMock, "u1")
		carrierMock.ExpectQuery(`FROM "platform_admins" WHERE user_id`).
			WillReturnError(errors.New("connection reset"))

		r := scopeProbeRouter(OptionalAuthMiddleware(userRepo, nil, nil, svc))
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("Authorization", "Bearer "+generateTestJWT(t, "u1"))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: this middleware never aborts (%s)", w.Code, w.Body.String())
		}
		if got := scopesFromBody(t, w.Body.String()); hasScope(got, "admin") {
			t.Errorf("scopes = %v: an unresolved authority question must not become an elevation", got)
		}
	})
}
