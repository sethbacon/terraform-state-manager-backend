// auth_oidc_nonce_pkce_test.go exercises the BeginAuth / WithExpectedNonce /
// WithPKCEVerifier hardening (GHSA-35rf-4r25-jxxv) end-to-end through the real
// HTTP handlers: LoginHandler generates a per-login nonce and PKCE verifier via
// BeginAuth and persists them in the state store; CallbackHandler must reject a
// callback whose ID token carries a different nonce, and accept one that
// matches.
package api

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/coreos/go-oidc/v3/oidc/oidctest"
	"github.com/gin-gonic/gin"

	"github.com/terraform-state-manager/terraform-state-manager/internal/auth"
	"github.com/terraform-state-manager/terraform-state-manager/internal/config"
)

// oidcTestIdP is a fake identity provider combining oidctest's discovery+JWKS
// endpoints (real discovery document, real signature verification against a
// generated RSA key pair) with a hand-rolled token endpoint. The token
// endpoint mints an ID token carrying whatever nonce the test wants embedded
// (letting a test drive both the matching and mismatched scenarios) and
// records the received code_verifier so a test can assert PKCE was actually
// sent on the exchange.
type oidcTestIdP struct {
	srv         *httptest.Server
	priv        *rsa.PrivateKey
	nonceToSend string
	gotVerifier string
}

func newOIDCTestIdP(t *testing.T) *oidcTestIdP {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey: %v", err)
	}
	idp := &oidcTestIdP{priv: priv}

	disc := &oidctest.Server{
		PublicKeys: []oidctest.PublicKey{
			{PublicKey: priv.Public(), KeyID: "test-key", Algorithm: "RS256"},
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		idp.gotVerifier = r.FormValue("code_verifier")

		claims := map[string]any{
			"iss":            idp.srv.URL,
			"aud":            "test-client",
			"sub":            "user-1",
			"email":          "user1@example.com",
			"email_verified": true,
			"exp":            time.Now().Add(time.Hour).Unix(),
			"iat":            time.Now().Unix(),
		}
		if idp.nonceToSend != "" {
			claims["nonce"] = idp.nonceToSend
		}
		raw, err := json.Marshal(claims)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		idToken := oidctest.SignIDToken(priv, "test-key", "RS256", string(raw))

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "test-access-token",
			"token_type":   "Bearer",
			"id_token":     idToken,
		})
	})
	mux.Handle("/", disc)

	idp.srv = httptest.NewServer(mux)
	t.Cleanup(idp.srv.Close)
	disc.SetIssuer(idp.srv.URL)
	return idp
}

// newOIDCCallbackEnv builds AuthHandlers wired to the live test IdP (via the
// same SetOIDCProvider seam the setup wizard and boot path use at runtime) and
// registers the real /login and /callback routes.
func newOIDCCallbackEnv(t *testing.T, idp *oidcTestIdP) (*sourcesEnv, *config.Config) {
	t.Helper()
	// idp.srv is a plaintext httptest.Server; NewOIDCProviderWithContext now
	// enforces RequireHTTPS outside DEV_MODE (issue #176), so exercise it here
	// the same way the auth package's own RequireHTTPS tests do.
	t.Setenv("DEV_MODE", "true")
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	// The exact internal call order of GetOrCreateUserFromOIDC / group-mapping /
	// scope resolution is an implementation detail of the identity library, not
	// something this test should be coupled to — match expectations by
	// regex+args regardless of order.
	mock.MatchExpectationsInOrder(false)

	cfg := &config.Config{}
	cfg.Server.PublicURL = "https://tsm.example.com"

	h, err := NewAuthHandlers(cfg, db)
	if err != nil {
		t.Fatalf("NewAuthHandlers: %v", err)
	}
	// With a non-nil DB the handlers wire the durable login-state store, whose
	// queries this sqlmock does not script; the store's own repository test
	// covers it. This test targets the nonce/PKCE round-trip, so the in-memory
	// implementation of the same contract keeps the flow observable.
	h.stateStore = auth.NewMemoryStateStore()

	ctx := context.Background()
	p, err := auth.NewOIDCProviderWithContext(ctx, &config.OIDCConfig{
		Enabled:      true,
		IssuerURL:    idp.srv.URL,
		ClientID:     "test-client",
		ClientSecret: "test-secret",
		RedirectURL:  "https://tsm.example.com/api/v1/auth/callback",
		Scopes:       []string{"openid", "email"},
	})
	if err != nil {
		t.Fatalf("NewOIDCProviderWithContext: %v", err)
	}
	h.SetOIDCProvider(p)

	r := gin.New()
	v1 := r.Group("/api/v1/auth")
	v1.GET("/login", h.LoginHandler())
	v1.GET("/callback", h.CallbackHandler())
	return &sourcesEnv{r: r, mock: mock}, cfg
}

// beginLogin drives a real /login request and extracts the state and nonce
// BeginAuth generated from the authorization redirect's Location header (the
// nonce is a plain query parameter on that URL — this test never needs to
// reach into the server's internal state store).
func beginLogin(t *testing.T, e *sourcesEnv) (state, nonce string) {
	t.Helper()
	w := e.do(http.MethodGet, "/api/v1/auth/login", "")
	if w.Code != http.StatusFound {
		t.Fatalf("login: status = %d, want 302 (%s)", w.Code, w.Body.String())
	}
	loc, err := url.Parse(w.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse Location: %v", err)
	}
	q := loc.Query()
	state = q.Get("state")
	nonce = q.Get("nonce")
	if state == "" || nonce == "" {
		t.Fatalf("login redirect missing state/nonce: %s", loc)
	}
	if q.Get("code_challenge") == "" || q.Get("code_challenge_method") != "S256" {
		t.Fatalf("login redirect missing PKCE code_challenge: %s", loc)
	}
	return state, nonce
}

// TestOIDCCallback_NonceMismatch_Rejected proves the binding actually works: an
// ID token whose nonce claim does not match the nonce BeginAuth generated for
// this login (simulating an injected/replayed token from a different login
// attempt) is rejected at the callback.
func TestOIDCCallback_NonceMismatch_Rejected(t *testing.T) {
	idp := newOIDCTestIdP(t)
	e, cfg := newOIDCCallbackEnv(t, idp)

	state, _ := beginLogin(t, e)

	// The IdP mints an ID token with a nonce the login never generated —
	// simulating a token injected/replayed from another login attempt.
	idp.nonceToSend = "attacker-controlled-nonce"

	w := e.do(http.MethodGet, "/api/v1/auth/callback?state="+state+"&code=test-code", "")
	if w.Code != http.StatusFound {
		t.Fatalf("callback: status = %d, want 302 redirect to the error page (%s)", w.Code, w.Body.String())
	}
	loc := w.Header().Get("Location")
	if !strings.HasPrefix(loc, cfg.Server.PublicURL+"/auth/callback?error=") {
		t.Fatalf("callback redirect = %q, want an error redirect", loc)
	}
	if !strings.Contains(loc, "id_token_invalid") {
		t.Errorf("callback redirect = %q, want error=id_token_invalid", loc)
	}
	if idp.gotVerifier == "" {
		t.Error("token endpoint never received a code_verifier (PKCE not sent)")
	}
}

// TestOIDCCallback_HappyPath_Succeeds is the happy path: an ID token carrying
// exactly the nonce BeginAuth generated for this login, exchanged with the
// matching PKCE verifier, completes the login and redirects to the frontend
// with a session cookie — proving BeginAuth/WithExpectedNonce/WithPKCEVerifier
// did not break the working flow.
func TestOIDCCallback_HappyPath_Succeeds(t *testing.T) {
	idp := newOIDCTestIdP(t)
	e, cfg := newOIDCCallbackEnv(t, idp)

	state, nonce := beginLogin(t, e)
	idp.nonceToSend = nonce

	// guardEmailRebind: no existing user bound to this email yet.
	e.mock.ExpectQuery("SELECT id, email, name, oidc_sub").WithArgs("user1@example.com").
		WillReturnRows(sqlmock.NewRows(idUserCols))
	// GetOrCreateUserFromOIDC: no existing user by oidc_sub, then re-checks by email.
	e.mock.ExpectQuery("SELECT id, email, name, oidc_sub").WithArgs("user-1").
		WillReturnRows(sqlmock.NewRows(idUserCols))
	e.mock.ExpectQuery("SELECT id, email, name, oidc_sub").WithArgs("user1@example.com").
		WillReturnRows(sqlmock.NewRows(idUserCols))
	e.mock.ExpectExec("INSERT INTO users").WillReturnResult(sqlmock.NewResult(0, 1))
	// effectiveOIDCGroupConfig: no admin-configured SSO overlay. Called twice —
	// once to resolve the group-claim name before ExtractGroups, once again
	// inside applyGroupMappings.
	e.mock.ExpectQuery("FROM sso_settings").WillReturnError(sql.ErrNoRows)
	e.mock.ExpectQuery("FROM sso_settings").WillReturnError(sql.ErrNoRows)
	// GetUserCombinedScopes -> GetUserMemberships: brand-new user, no memberships yet.
	e.mock.ExpectQuery("FROM organization_members om").WillReturnRows(sqlmock.NewRows(membershipCols))
	// audit.write
	e.mock.ExpectExec("INSERT INTO audit_logs").WillReturnResult(sqlmock.NewResult(0, 1))

	w := e.do(http.MethodGet, "/api/v1/auth/callback?state="+state+"&code=test-code", "")
	if w.Code != http.StatusFound {
		t.Fatalf("callback: status = %d, want 302 (%s)", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Location"); got != cfg.Server.PublicURL+"/auth/callback" {
		t.Errorf("callback redirect = %q, want the frontend callback with no error", got)
	}
	if idp.gotVerifier == "" {
		t.Error("token endpoint never received a code_verifier (PKCE not sent)")
	}

	var gotAuthCookie bool
	for _, ck := range w.Result().Cookies() {
		if ck.Name == "tsm_auth_token" && ck.Value != "" {
			gotAuthCookie = true
		}
	}
	if !gotAuthCookie {
		t.Error("callback did not set the session cookie on success")
	}
	if err := e.mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}
