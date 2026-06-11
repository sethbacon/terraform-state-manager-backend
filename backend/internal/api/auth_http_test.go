package api

import (
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"

	"github.com/terraform-state-manager/terraform-state-manager/internal/config"
)

// The JWT signing secret is resolved once per process — set it before any
// handler in this package's test binary can mint a session token.
func init() {
	os.Setenv("TSM_JWT_SECRET", "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
}

// newAuthEnv builds AuthHandlers (all SSO providers disabled) with an optional
// fake auth context, mirroring what AuthMiddleware would have established.
func newAuthEnv(t *testing.T, userID string, mutate func(*config.Config)) *sourcesEnv {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	cfg := &config.Config{}
	if mutate != nil {
		mutate(cfg)
	}
	h, err := NewAuthHandlers(cfg, db)
	if err != nil {
		t.Fatalf("NewAuthHandlers: %v", err)
	}

	r := gin.New()
	if userID != "" {
		r.Use(func(c *gin.Context) { c.Set("user_id", userID); c.Next() })
	}
	v1 := r.Group("/api/v1/auth")
	v1.GET("/providers", h.ProvidersHandler())
	v1.GET("/login", h.LoginHandler())
	v1.GET("/me", h.MeHandler())
	v1.POST("/refresh", h.RefreshHandler())
	v1.GET("/logout", h.LogoutHandler())
	return &sourcesEnv{r: r, mock: mock}
}

func TestProvidersHandler(t *testing.T) {
	e := newAuthEnv(t, "", nil)
	w := e.do(http.MethodGet, "/api/v1/auth/providers", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"providers":[]`) {
		t.Errorf("no providers configured should yield an empty list: %s", w.Body.String())
	}

	t.Setenv("DEV_MODE", "true")
	w = e.do(http.MethodGet, "/api/v1/auth/providers", "")
	if !strings.Contains(w.Body.String(), `"dev_mode":true`) {
		t.Errorf("dev_mode flag missing: %s", w.Body.String())
	}
}

func TestLoginHandler_NoProviders(t *testing.T) {
	e := newAuthEnv(t, "", nil)

	if w := e.do(http.MethodGet, "/api/v1/auth/login", ""); w.Code != http.StatusBadRequest {
		t.Errorf("OIDC unconfigured: status = %d, want 400", w.Code)
	}
	if w := e.do(http.MethodGet, "/api/v1/auth/login?provider=saml", ""); w.Code != http.StatusBadRequest {
		t.Errorf("SAML unconfigured: status = %d, want 400", w.Code)
	}
	if w := e.do(http.MethodGet, "/api/v1/auth/login?provider=saml:okta", ""); w.Code != http.StatusBadRequest {
		t.Errorf("named SAML IdP unconfigured: status = %d, want 400", w.Code)
	}
}

func TestMeHandler(t *testing.T) {
	// Anonymous: no auth context.
	anon := newAuthEnv(t, "", nil)
	if w := anon.do(http.MethodGet, "/api/v1/auth/me", ""); w.Code != http.StatusUnauthorized {
		t.Errorf("anonymous: status = %d, want 401", w.Code)
	}

	e := newAuthEnv(t, "u1", nil)
	now := time.Now()
	// GetUserWithOrgRoles = GetUserByID + GetUserMemberships.
	e.mock.ExpectQuery("SELECT id, email, name, oidc_sub").WithArgs("u1").
		WillReturnRows(sqlmock.NewRows(idUserCols).AddRow("u1", "a@b.c", "Alice", "sub-1", now, now))
	e.mock.ExpectQuery("FROM organization_members om").WithArgs("u1").
		WillReturnRows(sqlmock.NewRows(membershipCols).
			AddRow("o1", "default", nil, now, "editor", "Editor", []byte(`["state:read","state:write"]`)))
	// GetUserCombinedScopes re-reads memberships.
	e.mock.ExpectQuery("FROM organization_members om").WithArgs("u1").
		WillReturnRows(sqlmock.NewRows(membershipCols).
			AddRow("o1", "default", nil, now, "editor", "Editor", []byte(`["state:read","state:write"]`)))

	w := e.do(http.MethodGet, "/api/v1/auth/me", "")
	if w.Code != http.StatusOK {
		t.Fatalf("me: status = %d (%s)", w.Code, w.Body.String())
	}
	for _, want := range []string{`"a@b.c"`, `"allowed_scopes"`, `"state:write"`, `"memberships"`, `"default"`} {
		if !strings.Contains(w.Body.String(), want) {
			t.Errorf("me response missing %s: %s", want, w.Body.String())
		}
	}

	// Deleted user → 404.
	e.mock.ExpectQuery("SELECT id, email, name, oidc_sub").WithArgs("u1").
		WillReturnRows(sqlmock.NewRows(idUserCols))
	if w := e.do(http.MethodGet, "/api/v1/auth/me", ""); w.Code != http.StatusNotFound {
		t.Errorf("deleted user: status = %d, want 404", w.Code)
	}
}

func TestRefreshHandler(t *testing.T) {
	anon := newAuthEnv(t, "", nil)
	if w := anon.do(http.MethodPost, "/api/v1/auth/refresh", ""); w.Code != http.StatusUnauthorized {
		t.Errorf("anonymous: status = %d, want 401", w.Code)
	}

	e := newAuthEnv(t, "u1", nil)
	now := time.Now()
	e.mock.ExpectQuery("SELECT id, email, name, oidc_sub").WithArgs("u1").
		WillReturnRows(sqlmock.NewRows(idUserCols).AddRow("u1", "a@b.c", "Alice", "sub-1", now, now))
	e.mock.ExpectQuery("FROM organization_members om").WithArgs("u1").
		WillReturnRows(sqlmock.NewRows(membershipCols).
			AddRow("o1", "default", nil, now, "viewer", "Viewer", []byte(`["state:read"]`)))

	w := e.do(http.MethodPost, "/api/v1/auth/refresh", "")
	if w.Code != http.StatusOK {
		t.Fatalf("refresh: status = %d (%s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"expires_in"`) {
		t.Errorf("refresh must return the new TTL: %s", w.Body.String())
	}
	if strings.Contains(w.Body.String(), "eyJ") {
		t.Error("refresh leaked the JWT in the response body (must be cookie-only)")
	}

	cookies := w.Result().Cookies()
	var gotAuth, gotCSRF bool
	for _, ck := range cookies {
		switch ck.Name {
		case "tsm_auth_token":
			gotAuth = ck.HttpOnly && ck.Value != ""
		case "tsm_csrf":
			gotCSRF = ck.Value != ""
		}
	}
	if !gotAuth || !gotCSRF {
		t.Errorf("session cookies not set correctly (auth=%v csrf=%v)", gotAuth, gotCSRF)
	}

	// Vanished user → 401.
	e.mock.ExpectQuery("SELECT id, email, name, oidc_sub").WithArgs("u1").
		WillReturnRows(sqlmock.NewRows(idUserCols))
	if w := e.do(http.MethodPost, "/api/v1/auth/refresh", ""); w.Code != http.StatusUnauthorized {
		t.Errorf("vanished user: status = %d, want 401", w.Code)
	}
}

func TestLogoutHandler(t *testing.T) {
	e := newAuthEnv(t, "", func(cfg *config.Config) {
		cfg.Server.PublicURL = "https://tsm.example.com"
	})
	w := e.do(http.MethodGet, "/api/v1/auth/logout", "")
	if w.Code != http.StatusFound {
		t.Fatalf("logout: status = %d, want 302", w.Code)
	}
	if loc := w.Header().Get("Location"); loc != "https://tsm.example.com/" {
		t.Errorf("logout redirect = %q, want the frontend root", loc)
	}

	var authCleared bool
	for _, ck := range w.Result().Cookies() {
		if ck.Name == "tsm_auth_token" && ck.MaxAge < 0 {
			authCleared = true
			// Secure must match how it was set (https public URL → secure).
			if !ck.Secure {
				t.Error("clearing cookie lost the Secure attribute")
			}
		}
	}
	if !authCleared {
		t.Error("logout did not expire the auth cookie")
	}
}

func TestDeriveFrontendURL(t *testing.T) {
	cfg := &config.Config{}
	cfg.Server.PublicURL = "https://tsm.example.com/"
	if got := deriveFrontendURL(cfg); got != "https://tsm.example.com" {
		t.Errorf("public URL: %q", got)
	}

	cfg = &config.Config{}
	cfg.Auth.OIDC.RedirectURL = "https://app.example.com/auth/callback"
	if got := deriveFrontendURL(cfg); got != "https://app.example.com" {
		t.Errorf("redirect-derived: %q", got)
	}

	cfg = &config.Config{}
	cfg.Server.BaseURL = "http://localhost:8080/"
	if got := deriveFrontendURL(cfg); got != "http://localhost:8080" {
		t.Errorf("base fallback: %q", got)
	}
}
