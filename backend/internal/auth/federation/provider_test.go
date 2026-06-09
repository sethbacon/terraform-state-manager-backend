package federation

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeAssertionFile writes a mock projected SA OIDC token to a temp file and
// returns its path. The contents need not be a real JWT — the mock token
// endpoint never verifies it; that verification belongs to Entra in production.
func writeAssertionFile(t *testing.T, contents string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("writing assertion file: %v", err)
	}
	return path
}

// tokenEndpoint is an httptest-backed mock of the Entra token endpoint. It
// captures the last decoded form body and serves a configurable response.
type tokenEndpoint struct {
	t          *testing.T
	status     int
	body       string
	lastForm   url.Values
	lastMethod string
	callCount  int
}

func newTokenServer(t *testing.T, te *tokenEndpoint) *httptest.Server {
	t.Helper()
	te.t = t
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		te.callCount++
		te.lastMethod = r.Method
		_ = r.ParseForm()
		te.lastForm = r.PostForm
		w.Header().Set("Content-Type", "application/json")
		status := te.status
		if status == 0 {
			status = http.StatusOK
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(te.body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func testConfig(endpoint, tokenFile string) Config {
	return Config{
		TokenEndpoint: endpoint,
		ClientID:      "11111111-2222-3333-4444-555555555555",
		Scope:         "499b84ac-1321-427f-aa17-267ca6975798/.default",
		TokenFilePath: tokenFile,
	}
}

func TestStaticTokenProvider(t *testing.T) {
	p := StaticTokenProvider{Value: "static-bearer"}
	got, err := p.Token(context.Background())
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if got != "static-bearer" {
		t.Errorf("Token = %q, want static-bearer", got)
	}
}

func TestNewProvider_Validation(t *testing.T) {
	full := testConfig("https://endpoint", "/path/to/token")
	cases := map[string]func(*Config){
		"missing endpoint":  func(c *Config) { c.TokenEndpoint = "" },
		"missing client id": func(c *Config) { c.ClientID = "" },
		"missing scope":     func(c *Config) { c.Scope = "" },
		"missing tokenfile": func(c *Config) { c.TokenFilePath = "" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := full
			mutate(&cfg)
			if _, err := NewProvider(cfg, nil); err == nil {
				t.Errorf("expected validation error for %s", name)
			}
		})
	}
	if _, err := NewProvider(full, nil); err != nil {
		t.Errorf("unexpected error on valid config: %v", err)
	}
}

func TestFederatedTokenProvider_Exchange_Success(t *testing.T) {
	te := &tokenEndpoint{body: `{"token_type":"Bearer","access_token":"ado-access-token","expires_in":3600}`}
	srv := newTokenServer(t, te)
	tokenFile := writeAssertionFile(t, "mock.assertion.jwt\n")

	p, err := NewProvider(testConfig(srv.URL, tokenFile), srv.Client())
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}

	got, err := p.Token(context.Background())
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if got != "ado-access-token" {
		t.Errorf("Token = %q, want ado-access-token", got)
	}

	// Assert the exchange request used the WIF client-assertion grant.
	if te.lastMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", te.lastMethod)
	}
	if g := te.lastForm.Get("grant_type"); g != "client_credentials" {
		t.Errorf("grant_type = %q, want client_credentials", g)
	}
	if a := te.lastForm.Get("client_assertion_type"); a != clientAssertionType {
		t.Errorf("client_assertion_type = %q, want %q", a, clientAssertionType)
	}
	// The projected token is sent as the assertion, trimmed of trailing newline.
	if a := te.lastForm.Get("client_assertion"); a != "mock.assertion.jwt" {
		t.Errorf("client_assertion = %q, want mock.assertion.jwt", a)
	}
	if c := te.lastForm.Get("client_id"); c != "11111111-2222-3333-4444-555555555555" {
		t.Errorf("client_id = %q", c)
	}
	if s := te.lastForm.Get("scope"); s != "499b84ac-1321-427f-aa17-267ca6975798/.default" {
		t.Errorf("scope = %q", s)
	}
}

func TestFederatedTokenProvider_Caches(t *testing.T) {
	te := &tokenEndpoint{body: `{"access_token":"cached-token","expires_in":3600}`}
	srv := newTokenServer(t, te)
	tokenFile := writeAssertionFile(t, "assertion")

	p, err := NewProvider(testConfig(srv.URL, tokenFile), srv.Client())
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}

	for i := 0; i < 3; i++ {
		if _, err := p.Token(context.Background()); err != nil {
			t.Fatalf("Token call %d: %v", i, err)
		}
	}
	if te.callCount != 1 {
		t.Errorf("token endpoint called %d times, want 1 (cached)", te.callCount)
	}
}

func TestFederatedTokenProvider_RefreshesAfterExpiry(t *testing.T) {
	te := &tokenEndpoint{body: `{"access_token":"short-lived","expires_in":120}`}
	srv := newTokenServer(t, te)
	tokenFile := writeAssertionFile(t, "assertion")

	p, err := NewProvider(testConfig(srv.URL, tokenFile), srv.Client())
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}

	// Drive the injectable clock past the cached-until point. With expires_in=120
	// and a 60s leeway, the cached window is ~60s; advance well beyond it.
	base := time.Now()
	p.now = func() time.Time { return base }
	if _, err := p.Token(context.Background()); err != nil {
		t.Fatalf("first Token: %v", err)
	}
	p.now = func() time.Time { return base.Add(2 * time.Minute) }
	if _, err := p.Token(context.Background()); err != nil {
		t.Fatalf("second Token: %v", err)
	}
	if te.callCount != 2 {
		t.Errorf("token endpoint called %d times, want 2 (re-exchanged after expiry)", te.callCount)
	}
}

func TestFederatedTokenProvider_OAuthError(t *testing.T) {
	te := &tokenEndpoint{
		status: http.StatusBadRequest,
		body:   `{"error":"invalid_client","error_description":"AADSTS700213: no federated credential matched"}`,
	}
	srv := newTokenServer(t, te)
	tokenFile := writeAssertionFile(t, "assertion")

	p, err := NewProvider(testConfig(srv.URL, tokenFile), srv.Client())
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	if _, err := p.Token(context.Background()); err == nil {
		t.Fatal("expected error on OAuth failure")
	}
}

func TestFederatedTokenProvider_MissingTokenFile(t *testing.T) {
	te := &tokenEndpoint{body: `{"access_token":"x","expires_in":3600}`}
	srv := newTokenServer(t, te)

	p, err := NewProvider(testConfig(srv.URL, filepath.Join(t.TempDir(), "does-not-exist")), srv.Client())
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	if _, err := p.Token(context.Background()); err == nil {
		t.Fatal("expected error when projected token file is missing")
	}
}

func TestFederatedTokenProvider_EmptyAccessToken(t *testing.T) {
	te := &tokenEndpoint{body: `{"token_type":"Bearer","expires_in":3600}`}
	srv := newTokenServer(t, te)
	tokenFile := writeAssertionFile(t, "assertion")

	p, err := NewProvider(testConfig(srv.URL, tokenFile), srv.Client())
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	if _, err := p.Token(context.Background()); err == nil {
		t.Fatal("expected error when response carries no access_token")
	}
}
