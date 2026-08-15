package middleware

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
	idauth "github.com/sethbacon/terraform-suite-identity/identity/auth"
	idstore "github.com/sethbacon/terraform-suite-identity/identity/store"

	"github.com/terraform-state-manager/terraform-state-manager/internal/approles"
	"github.com/terraform-state-manager/terraform-state-manager/internal/auth"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
)

// The signing secret is resolved once per process; set it before any test can
// trigger auth.ValidateJWTSecret.
func init() {
	os.Setenv("TSM_JWT_SECRET", "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
}

var userCols = []string{"id", "email", "name", "oidc_sub", "created_at", "updated_at"}

func newUserRepo(t *testing.T) (*idstore.UserRepository, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New (user): %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return idstore.NewUserRepository(db), mock
}

func newTokenRepo(t *testing.T) (*idstore.TokenRepository, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New (token): %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return idstore.NewTokenRepository(db), mock
}

func generateTestJWT(t *testing.T, userID string) string {
	t.Helper()
	token, err := auth.GenerateJWT(userID, "test@example.com", []string{"state:read"}, time.Hour)
	if err != nil {
		t.Fatalf("GenerateJWT: %v", err)
	}
	return token
}

// expectNotRevoked queues the revocation check for any JTI.
func expectNotRevoked(mock sqlmock.Sqlmock, revoked bool) {
	mock.ExpectQuery("SELECT EXISTS").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(revoked))
}

func expectUserFound(mock sqlmock.Sqlmock, userID string) {
	now := time.Now()
	mock.ExpectQuery("SELECT id, email, name, oidc_sub, created_at, updated_at").
		WillReturnRows(sqlmock.NewRows(userCols).
			AddRow(userID, "test@example.com", "Test User", "sub-1", now, now))
}

// authRouter wires AuthMiddleware plus a probe handler that reports the auth
// context the middleware established.
func authRouter(userRepo *idstore.UserRepository, tokenRepo *idstore.TokenRepository) *gin.Engine {
	return authRouterWithKeys(userRepo, tokenRepo, nil)
}

func authRouterWithKeys(userRepo *idstore.UserRepository, tokenRepo *idstore.TokenRepository, keyRepo *idstore.APIKeyRepository) *gin.Engine {
	r := gin.New()
	r.Use(AuthMiddleware(userRepo, tokenRepo, keyRepo, nil, nil, nil))
	r.GET("/", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"user_id":     c.GetString("user_id"),
			"auth_method": c.GetString("auth_method"),
		})
	})
	return r
}

func TestAuthMiddleware_MissingCredentials(t *testing.T) {
	// nil repos are safe: the middleware aborts before any repo call.
	r := authRouter(nil, nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func TestAuthMiddleware_InvalidToken(t *testing.T) {
	r := authRouter(nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer not-a-valid-token")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func TestAuthMiddleware_MTLSPreAuthPassesThrough(t *testing.T) {
	r := gin.New()
	// Simulates mtls.AuthMiddleware running earlier in the chain.
	r.Use(func(c *gin.Context) { c.Set("auth_method", "mtls"); c.Next() })
	r.Use(AuthMiddleware(nil, nil, nil, nil, nil, nil))
	r.GET("/", func(c *gin.Context) { c.Status(http.StatusOK) })

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (mTLS pre-auth should bypass JWT)", w.Code)
	}
}

func TestAuthMiddleware_ValidBearerToken(t *testing.T) {
	userRepo, userMock := newUserRepo(t)
	tokenRepo, tokenMock := newTokenRepo(t)
	expectNotRevoked(tokenMock, false)
	expectUserFound(userMock, "user-1")

	r := authRouter(userRepo, tokenRepo)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+generateTestJWT(t, "user-1"))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !contains(body, `"user_id":"user-1"`) {
		t.Errorf("user_id not set in context: %s", body)
	}
	if !contains(body, `"auth_method":"jwt"`) {
		t.Errorf("auth_method = %s, want jwt (header token)", body)
	}
}

func TestAuthMiddleware_ValidCookieToken(t *testing.T) {
	userRepo, userMock := newUserRepo(t)
	tokenRepo, tokenMock := newTokenRepo(t)
	expectNotRevoked(tokenMock, false)
	expectUserFound(userMock, "user-1")

	r := authRouter(userRepo, tokenRepo)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: AuthCookieName, Value: generateTestJWT(t, "user-1")})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	if !contains(w.Body.String(), `"auth_method":"jwt_cookie"`) {
		t.Errorf("auth_method = %s, want jwt_cookie", w.Body.String())
	}
}

func TestAuthMiddleware_RevokedToken(t *testing.T) {
	userRepo, _ := newUserRepo(t)
	tokenRepo, tokenMock := newTokenRepo(t)
	expectNotRevoked(tokenMock, true)

	r := authRouter(userRepo, tokenRepo)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+generateTestJWT(t, "user-1"))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 for a revoked token", w.Code)
	}
}

func TestAuthMiddleware_UserNotFound(t *testing.T) {
	userRepo, userMock := newUserRepo(t)
	tokenRepo, tokenMock := newTokenRepo(t)
	expectNotRevoked(tokenMock, false)
	// No rows → GetUserByID returns (nil, nil).
	userMock.ExpectQuery("SELECT id, email, name, oidc_sub, created_at, updated_at").
		WillReturnRows(sqlmock.NewRows(userCols))

	r := authRouter(userRepo, tokenRepo)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+generateTestJWT(t, "ghost"))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 for a deleted user", w.Code)
	}
}

func TestOptionalAuthMiddleware_NoToken(t *testing.T) {
	r := gin.New()
	r.Use(OptionalAuthMiddleware(nil, nil, nil, nil))
	r.GET("/", func(c *gin.Context) {
		_, authed := c.Get("user_id")
		c.JSON(http.StatusOK, gin.H{"authed": authed})
	})

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if !contains(w.Body.String(), `"authed":false`) {
		t.Errorf("expected unauthenticated pass-through, got %s", w.Body.String())
	}
}

func TestOptionalAuthMiddleware_InvalidTokenStillPasses(t *testing.T) {
	r := gin.New()
	r.Use(OptionalAuthMiddleware(nil, nil, nil, nil))
	r.GET("/", func(c *gin.Context) { c.Status(http.StatusOK) })

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer garbage")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (optional auth never aborts)", w.Code)
	}
}

func TestOptionalAuthMiddleware_ValidToken(t *testing.T) {
	userRepo, userMock := newUserRepo(t)
	tokenRepo, tokenMock := newTokenRepo(t)
	expectNotRevoked(tokenMock, false)
	expectUserFound(userMock, "user-1")

	r := gin.New()
	r.Use(OptionalAuthMiddleware(userRepo, tokenRepo, nil, nil))
	r.GET("/", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"user_id": c.GetString("user_id")})
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+generateTestJWT(t, "user-1"))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if !contains(w.Body.String(), `"user_id":"user-1"`) {
		t.Errorf("expected populated auth context, got %s", w.Body.String())
	}
}

func contains(s, substr string) bool {
	return strings.Contains(s, substr)
}

// --- API-key authentication ---

var apiKeyCols = []string{"id", "user_id", "organization_id", "name", "description", "key_hash",
	"key_prefix", "scopes", "expires_at", "last_used_at", "expiry_notification_sent_at", "created_at"}

func newAPIKeyRepo(t *testing.T) (*idstore.APIKeyRepository, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New (apikey): %v", err)
	}
	mock.MatchExpectationsInOrder(false) // last-used update is async
	t.Cleanup(func() { db.Close() })
	return idstore.NewAPIKeyRepository(db), mock
}

func mintTestKey(t *testing.T) (fullKey, hash, prefix string) {
	t.Helper()
	fullKey, hash, prefix, err := idauth.GenerateAPIKey(APIKeyPrefix)
	if err != nil {
		t.Fatalf("GenerateAPIKey: %v", err)
	}
	return fullKey, hash, prefix
}

func keyRow(hash, prefix string, scopes string, expires *time.Time) *sqlmock.Rows {
	return sqlmock.NewRows(apiKeyCols).
		AddRow("k1", "u1", "org1", "ci", nil, hash, prefix, []byte(scopes), expires, nil, nil, time.Now())
}

func TestAuthMiddleware_APIKeyAuthenticates(t *testing.T) {
	fullKey, hash, prefix := mintTestKey(t)
	keyRepo, keyMock := newAPIKeyRepo(t)
	userRepo, userMock := newUserRepo(t)
	// The prefix lookup binds a row cap as of identity v0.25.0: one prefix
	// matching more than 100 live keys is refused outright rather than fanned
	// across the whole table as bcrypt candidates.
	keyMock.ExpectQuery("FROM api_keys").WithArgs(prefix, sqlmock.AnyArg()).
		WillReturnRows(keyRow(hash, prefix, `["state:read","state:drift"]`, nil))
	// Async last-used update may or may not land before the test ends.
	keyMock.ExpectExec("UPDATE api_keys").WillReturnResult(sqlmock.NewResult(0, 1))
	expectUserFound(userMock, "u1")

	r := authRouterWithKeys(userRepo, nil, keyRepo)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+fullKey)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"auth_method":"apikey"`) || !strings.Contains(w.Body.String(), `"user_id":"u1"`) {
		t.Errorf("auth context = %s", w.Body.String())
	}
}

func newOrgRepoMW(t *testing.T) (*approles.Members, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New (org): %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return approles.NewMembers(db, nil, approles.RoleSourceIdentity), mock
}

// mwMembershipCols mirrors GetUserCombinedScopes' membership projection.
var mwMembershipCols = []string{"organization_id", "organization_name", "role_template_id",
	"created_at", "role_template_name", "role_template_display_name", "role_template_scopes"}

// TestAuthMiddleware_APIKeyScopesCappedByLiveOwnerScopes proves an API key's
// stored scopes are intersected with the owner's CURRENT combined scopes at auth
// time, so a key minted while the owner held admin no longer carries admin after
// the owner is downgraded (#223). The key statically carries admin+state:read,
// but the owner's live scopes are only state:read, so admin must be dropped while
// state:read is retained.
func TestAuthMiddleware_APIKeyScopesCappedByLiveOwnerScopes(t *testing.T) {
	fullKey, hash, prefix := mintTestKey(t)
	keyRepo, keyMock := newAPIKeyRepo(t)
	userRepo, userMock := newUserRepo(t)
	orgRepo, orgMock := newOrgRepoMW(t)
	// The prefix lookup binds a row cap as of identity v0.25.0: one prefix
	// matching more than 100 live keys is refused outright rather than fanned
	// across the whole table as bcrypt candidates.
	keyMock.ExpectQuery("FROM api_keys").WithArgs(prefix, sqlmock.AnyArg()).
		WillReturnRows(keyRow(hash, prefix, `["admin","state:read"]`, nil))
	keyMock.ExpectExec("UPDATE api_keys").WillReturnResult(sqlmock.NewResult(0, 1))
	expectUserFound(userMock, "u1")
	// Owner is now only a viewer (state:read) — admin was stripped upstream.
	orgMock.ExpectQuery("FROM organization_members om").WithArgs("u1").
		WillReturnRows(sqlmock.NewRows(mwMembershipCols).
			AddRow("o1", "default", nil, time.Now(), "viewer", "Viewer", []byte(`["state:read"]`)))

	r := gin.New()
	r.Use(AuthMiddleware(userRepo, nil, keyRepo, orgRepo, nil, nil))
	r.GET("/", func(c *gin.Context) {
		sc, _ := c.Get("scopes")
		c.JSON(http.StatusOK, gin.H{"scopes": sc})
	})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+fullKey)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if strings.Contains(body, "admin") {
		t.Errorf("downgraded owner: key must not retain admin scope: %s", body)
	}
	if !strings.Contains(body, "state:read") {
		t.Errorf("key should retain scopes the owner still holds: %s", body)
	}
}

func TestAuthMiddleware_APIKeyRejections(t *testing.T) {
	fullKey, hash, prefix := mintTestKey(t)

	// Wrong secret under a colliding prefix: bcrypt mismatch -> 401.
	keyRepo, keyMock := newAPIKeyRepo(t)
	keyMock.ExpectQuery("FROM api_keys").
		WillReturnRows(keyRow(hash, prefix, `["state:read"]`, nil))
	r := authRouterWithKeys(nil, nil, keyRepo)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+fullKey[:len(fullKey)-2]+"xx")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("tampered key: status = %d, want 401", w.Code)
	}

	// Expired key: the matching hash must NOT authenticate.
	keyRepo2, keyMock2 := newAPIKeyRepo(t)
	past := time.Now().Add(-time.Hour)
	keyMock2.ExpectQuery("FROM api_keys").
		WillReturnRows(keyRow(hash, prefix, `["state:read"]`, &past))
	r2 := authRouterWithKeys(nil, nil, keyRepo2)
	req2 := httptest.NewRequest(http.MethodGet, "/", nil)
	req2.Header.Set("Authorization", "Bearer "+fullKey)
	w2 := httptest.NewRecorder()
	r2.ServeHTTP(w2, req2)
	if w2.Code != http.StatusUnauthorized {
		t.Errorf("expired key: status = %d, want 401", w2.Code)
	}

	// A key whose owning user was deleted is orphaned -> 401.
	keyRepo3, keyMock3 := newAPIKeyRepo(t)
	keyMock3.ExpectQuery("FROM api_keys").
		WillReturnRows(keyRow(hash, prefix, `["state:read"]`, nil))
	userRepo3, userMock3 := newUserRepo(t)
	userMock3.ExpectQuery("SELECT id, email, name, oidc_sub, created_at, updated_at").
		WillReturnRows(sqlmock.NewRows(userCols))
	r3 := authRouterWithKeys(userRepo3, nil, keyRepo3)
	req3 := httptest.NewRequest(http.MethodGet, "/", nil)
	req3.Header.Set("Authorization", "Bearer "+fullKey)
	w3 := httptest.NewRecorder()
	r3.ServeHTTP(w3, req3)
	if w3.Code != http.StatusUnauthorized {
		t.Errorf("orphaned key: status = %d, want 401", w3.Code)
	}

	// Keys never authenticate from cookies (API keys are header-only).
	keyRepo4, _ := newAPIKeyRepo(t)
	r4 := authRouterWithKeys(nil, nil, keyRepo4)
	req4 := httptest.NewRequest(http.MethodGet, "/", nil)
	req4.AddCookie(&http.Cookie{Name: AuthCookieName, Value: fullKey})
	w4 := httptest.NewRecorder()
	r4.ServeHTTP(w4, req4)
	if w4.Code != http.StatusUnauthorized {
		t.Errorf("cookie-presented key: status = %d, want 401", w4.Code)
	}
}

// --- Per-user revoke-all watermark (#330) ---
//
// A JWT freezes its scopes at login, so an authority reduction that happens
// afterwards is invisible to it, and the JTI denylist cannot help because a
// membership removal knows no JTIs. These tests cover the use-time half of the
// fix: the watermark the credential sweep writes must actually stop a
// pre-existing session.

func newUserRevocationRepo(t *testing.T) (*repositories.UserTokenRevocationRepository, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New (revocations): %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return repositories.NewUserTokenRevocationRepository(db), mock
}

// expectWatermark queues the per-user revoke-all lookup.
func expectWatermark(mock sqlmock.Sqlmock, revoked bool) {
	mock.ExpectQuery("FROM user_token_revocations").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(revoked))
}

func watermarkRouter(userRepo *idstore.UserRepository, tokenRepo *idstore.TokenRepository, rev *repositories.UserTokenRevocationRepository) *gin.Engine {
	r := gin.New()
	r.Use(AuthMiddleware(userRepo, tokenRepo, nil, nil, rev, nil))
	r.GET("/", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"user_id": c.GetString("user_id")}) })
	return r
}

func TestAuthMiddleware_WatermarkRevokesPreExistingSession(t *testing.T) {
	userRepo, _ := newUserRepo(t)
	tokenRepo, tokenMock := newTokenRepo(t)
	revRepo, revMock := newUserRevocationRepo(t)
	expectNotRevoked(tokenMock, false) // the JTI itself was never denylisted
	expectWatermark(revMock, true)     // ...but the user's authority was reduced

	r := watermarkRouter(userRepo, tokenRepo, revRepo)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+generateTestJWT(t, "user-1"))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (a session predating the watermark must be rejected): %s", w.Code, w.Body.String())
	}
	if err := revMock.ExpectationsWereMet(); err != nil {
		t.Errorf("watermark was never consulted: %v", err)
	}
}

func TestAuthMiddleware_WatermarkNotSetAllowsSession(t *testing.T) {
	userRepo, userMock := newUserRepo(t)
	tokenRepo, tokenMock := newTokenRepo(t)
	revRepo, revMock := newUserRevocationRepo(t)
	expectNotRevoked(tokenMock, false)
	expectWatermark(revMock, false)
	expectUserFound(userMock, "user-1")

	r := watermarkRouter(userRepo, tokenRepo, revRepo)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+generateTestJWT(t, "user-1"))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (no reduction, session stands): %s", w.Code, w.Body.String())
	}
}

// A watermark lookup that errors must not be read as "not revoked": the
// revocation status is unknown, so the request fails closed.
func TestAuthMiddleware_WatermarkLookupErrorFailsClosed(t *testing.T) {
	userRepo, _ := newUserRepo(t)
	tokenRepo, tokenMock := newTokenRepo(t)
	revRepo, revMock := newUserRevocationRepo(t)
	expectNotRevoked(tokenMock, false)
	revMock.ExpectQuery("FROM user_token_revocations").WillReturnError(sql.ErrConnDone)

	r := watermarkRouter(userRepo, tokenRepo, revRepo)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+generateTestJWT(t, "user-1"))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (unknown revocation status must not authenticate)", w.Code)
	}
}

func TestOptionalAuthMiddleware_WatermarkLeavesRequestUnauthenticated(t *testing.T) {
	userRepo, _ := newUserRepo(t)
	tokenRepo, tokenMock := newTokenRepo(t)
	revRepo, revMock := newUserRevocationRepo(t)
	expectNotRevoked(tokenMock, false)
	expectWatermark(revMock, true)

	r := gin.New()
	r.Use(OptionalAuthMiddleware(userRepo, tokenRepo, revRepo, nil))
	r.GET("/", func(c *gin.Context) {
		_, authed := c.Get("user_id")
		c.JSON(http.StatusOK, gin.H{"authed": authed})
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+generateTestJWT(t, "user-1"))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (optional auth never aborts)", w.Code)
	}
	if !contains(w.Body.String(), `"authed":false`) {
		t.Errorf("a revoked session must not be treated as authenticated: %s", w.Body.String())
	}
}
