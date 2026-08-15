package api

import (
	"encoding/json"
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
	h, err := NewAuthHandlers(cfg, db, nil)
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
	v1.POST("/logout", h.LogoutPostHandler())
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

// GUARD me-allowed-scopes-carrier (#391).
//
// allowed_scopes was built solely from the role-template union
// (approles.Members.GetUserCombinedScopes) while platform-admin authority has
// had a SECOND carrier since migration 000030: the platform_admins table, which
// middleware.AuthMiddleware resolves per request and publishes as `scopes`. A
// carrier-only administrator was therefore authorized at every server-side
// HasScope(ScopeAdmin) site and reported to their own session as an ordinary
// user, and the frontend gates all admin navigation on this field.
//
// TSM had no cover for this: migration 000030 says NO BACKFILL, so the carrier
// and the role union are independent from the very first grant — where
// registry's 000051 backfilled and the two agreed until it retired the union's
// `admin` entirely.
//
// The subtraction cases below are the ones TSM's ADDITIVE model leaves: a
// role-template `admin` is real authority here and survives, but one the request
// does not actually carry does not. That is not a hypothetical — the API-key
// path strips `admin` unconditionally, so without it /auth/me tells an
// administrator's CI token that it is an administrator.
func TestMeHandlerReportsTheAdminScopeInForceForTheRequest(t *testing.T) {
	tests := []struct {
		name string
		// effective is what the auth middleware published for this request.
		effective []string
		setScopes bool
		// union is the role-template scope set in this application's tables.
		union     string
		wantAdmin bool
	}{
		{
			name:      "carrier-only administrator",
			effective: []string{"state:read", "admin"},
			setScopes: true,
			union:     `["state:read"]`,
			wantAdmin: true,
		},
		{
			name:      "ordinary user is not elevated",
			effective: []string{"state:read"},
			setScopes: true,
			union:     `["state:read"]`,
			wantAdmin: false,
		},
		{
			// ADDITIVE: unlike registry, a role-template `admin` still confers
			// here, and stripping it would be the same defect pointed the other
			// way — an administrator shown no admin UI.
			name:      "role-template administrator with no carrier row",
			effective: []string{"state:read", "admin"},
			setScopes: true,
			union:     `["state:read","admin"]`,
			wantAdmin: true,
		},
		{
			// THE SUBTRACTION. The union says admin; this request does not carry
			// it (the API-key path strips it, and a template granted since the
			// token was minted is not in it either). Reporting it would render
			// the whole admin navigation for a principal whose every admin
			// request answers 403.
			name:      "admin-bearing role template the request does not carry",
			effective: []string{"state:read"},
			setScopes: true,
			union:     `["state:read","admin"]`,
			wantAdmin: false,
		},
		{
			// Fail closed: every authenticated path publishes `scopes`, so an
			// absent one is a mis-wired route rather than a principal.
			name:      "no scopes published on the request",
			setScopes: false,
			union:     `["state:read"]`,
			wantAdmin: false,
		},
		{
			name:      "no scopes published, admin-bearing role template",
			setScopes: false,
			union:     `["admin"]`,
			wantAdmin: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatalf("sqlmock.New: %v", err)
			}
			t.Cleanup(func() { db.Close() })
			h, err := NewAuthHandlers(&config.Config{}, db, nil)
			if err != nil {
				t.Fatalf("NewAuthHandlers: %v", err)
			}
			r := gin.New()
			r.Use(func(c *gin.Context) {
				c.Set("user_id", "u1")
				if tt.setScopes {
					c.Set("scopes", tt.effective)
				}
				c.Next()
			})
			r.GET("/api/v1/auth/me", h.MeHandler())
			e := &sourcesEnv{r: r, mock: mock}

			now := time.Now()
			mock.ExpectQuery("SELECT id, email, name, oidc_sub").WithArgs("u1").
				WillReturnRows(sqlmock.NewRows(idUserCols).AddRow("u1", "a@b.c", "Alice", "sub-1", now, now))
			mock.ExpectQuery("FROM organization_members om").WithArgs("u1").
				WillReturnRows(sqlmock.NewRows(membershipCols).
					AddRow("o1", "default", nil, now, "editor", "Editor", []byte(tt.union)))
			mock.ExpectQuery("FROM organization_members om").WithArgs("u1").
				WillReturnRows(sqlmock.NewRows(membershipCols).
					AddRow("o1", "default", nil, now, "editor", "Editor", []byte(tt.union)))

			w := e.do(http.MethodGet, "/api/v1/auth/me", "")
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d (%s)", w.Code, w.Body.String())
			}
			var got struct {
				AllowedScopes []string `json:"allowed_scopes"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode: %v", err)
			}

			gotAdmin := false
			admins := 0
			for _, s := range got.AllowedScopes {
				if s == "admin" {
					gotAdmin = true
					admins++
				}
			}
			if gotAdmin != tt.wantAdmin {
				t.Errorf("allowed_scopes = %v, admin present = %v, want %v",
					got.AllowedScopes, gotAdmin, tt.wantAdmin)
			}
			if admins > 1 {
				t.Errorf("allowed_scopes = %v: admin reported %d times", got.AllowedScopes, admins)
			}
			// The finer scopes have one carrier and must survive untouched: this
			// reconciles the `admin` bit, it does not re-derive the union.
			if strings.Contains(tt.union, "state:read") {
				found := false
				for _, s := range got.AllowedScopes {
					if s == "state:read" {
						found = true
					}
				}
				if !found {
					t.Errorf("allowed_scopes = %v: the role union's own scopes were dropped", got.AllowedScopes)
				}
			}
		})
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

// The GET verb is gone (#274). CSRFProtect skips safe methods, so a GET logout
// sits outside the double-submit check and a cross-site link can force a
// victim's session to be revoked. This asserts the route is absent rather than
// merely unused — re-adding it reopens the vector.
func TestLogoutGetRouteRemoved(t *testing.T) {
	e := newAuthEnv(t, "", func(cfg *config.Config) {
		cfg.Server.PublicURL = "https://tsm.example.com"
	})
	w := e.do(http.MethodGet, "/api/v1/auth/logout", "")
	if w.Code != http.StatusNotFound && w.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET logout: status = %d, want 404/405 — the forgeable verb must not exist", w.Code)
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

// POST /logout is the CSRF-safe counterpart to the GET route (#274). It does the
// same session teardown but answers 200 with the post-logout destination in the
// body instead of a 302 — an XHR cannot usefully follow a cross-origin redirect
// to the IdP's end-session endpoint, so the SPA has to navigate itself.
func TestLogoutPostHandler(t *testing.T) {
	e := newAuthEnv(t, "", func(cfg *config.Config) {
		cfg.Server.PublicURL = "https://tsm.example.com"
	})
	w := e.do(http.MethodPost, "/api/v1/auth/logout", "")
	if w.Code != http.StatusOK {
		t.Fatalf("logout POST: status = %d, want 200", w.Code)
	}
	if w.Header().Get("Location") != "" {
		t.Error("logout POST must not redirect; the SPA navigates using redirect_url")
	}
	if !strings.Contains(w.Body.String(), `"redirect_url":"https://tsm.example.com/"`) {
		t.Errorf("body = %s, want a redirect_url pointing at the frontend root", w.Body.String())
	}

	var authCleared bool
	for _, ck := range w.Result().Cookies() {
		if ck.Name == "tsm_auth_token" && ck.MaxAge < 0 {
			authCleared = true
			if !ck.Secure {
				t.Error("clearing cookie lost the Secure attribute")
			}
		}
	}
	if !authCleared {
		t.Error("logout POST did not expire the auth cookie")
	}
}
