package middleware

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
	idstore "github.com/sethbacon/terraform-suite-identity/identity/store"

	"github.com/terraform-state-manager/terraform-state-manager/internal/auth"
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
	r := gin.New()
	r.Use(AuthMiddleware(userRepo, tokenRepo))
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
	r.Use(AuthMiddleware(nil, nil))
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
	r.Use(OptionalAuthMiddleware(nil, nil))
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
	r.Use(OptionalAuthMiddleware(nil, nil))
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
	r.Use(OptionalAuthMiddleware(userRepo, tokenRepo))
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
