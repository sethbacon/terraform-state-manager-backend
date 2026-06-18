package pipelines

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// resetEntraCache clears the package token cache so tests don't see each other's
// cached tokens.
func resetEntraCache() {
	entraCacheMu.Lock()
	entraCache = map[string]entraCachedToken{}
	entraCacheMu.Unlock()
}

func TestMintEntraADOToken_RequiresAllFields(t *testing.T) {
	resetEntraCache()
	for _, c := range []EntraCreds{
		{ClientID: "c", ClientSecret: "s"},
		{TenantID: "t", ClientSecret: "s"},
		{TenantID: "t", ClientID: "c"},
		{},
	} {
		if _, err := MintEntraADOToken(context.Background(), c); err == nil {
			t.Errorf("MintEntraADOToken(%+v) = nil error, want error", c)
		}
	}
}

func TestMintEntraADOToken_MintsAndCaches(t *testing.T) {
	resetEntraCache()
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if !strings.Contains(r.URL.Path, "/the-tenant/oauth2/v2.0/token") {
			t.Errorf("path = %s, want tenant token endpoint", r.URL.Path)
		}
		_ = r.ParseForm()
		if r.Form.Get("grant_type") != "client_credentials" {
			t.Errorf("grant_type = %q", r.Form.Get("grant_type"))
		}
		if !strings.HasSuffix(r.Form.Get("scope"), "/.default") {
			t.Errorf("scope = %q, want .default", r.Form.Get("scope"))
		}
		if r.Form.Get("client_id") != "the-client" || r.Form.Get("client_secret") == "" {
			t.Errorf("client creds not forwarded: %q / %q", r.Form.Get("client_id"), r.Form.Get("client_secret"))
		}
		_, _ = w.Write([]byte(`{"access_token":"app-token-123","expires_in":3600}`))
	}))
	defer srv.Close()

	old := entraLoginBaseURL
	entraLoginBaseURL = srv.URL
	defer func() { entraLoginBaseURL = old }()

	creds := EntraCreds{TenantID: "the-tenant", ClientID: "the-client", ClientSecret: "the-secret"}
	tok, err := MintEntraADOToken(context.Background(), creds)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if tok != "app-token-123" {
		t.Fatalf("token = %q, want app-token-123", tok)
	}
	// Second call (well within the hour) is served from cache: no new HTTP call.
	if _, err := MintEntraADOToken(context.Background(), creds); err != nil {
		t.Fatalf("mint (cached): %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("token endpoint hit %d times, want 1 (cache miss then hit)", got)
	}

	// Rotating the secret yields a different cache key → a fresh mint.
	rotated := EntraCreds{TenantID: "the-tenant", ClientID: "the-client", ClientSecret: "new-secret"}
	if _, err := MintEntraADOToken(context.Background(), rotated); err != nil {
		t.Fatalf("mint (rotated): %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("token endpoint hit %d times after rotation, want 2", got)
	}
}

func TestMintEntraADOToken_MapsErrorStatus(t *testing.T) {
	resetEntraCache()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid_client","error_description":"AADSTS7000215"}`))
	}))
	defer srv.Close()

	old := entraLoginBaseURL
	entraLoginBaseURL = srv.URL
	defer func() { entraLoginBaseURL = old }()

	_, err := MintEntraADOToken(context.Background(),
		EntraCreds{TenantID: "t", ClientID: "c", ClientSecret: "bad"})
	if err == nil {
		t.Fatal("expected error on 401")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("error = %v, want it to mention status 401", err)
	}
}

func TestADOToken_AuthHeader(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet, "https://example.test", nil)
	ADOPAT("the-pat").authorize(req)
	if got := req.Header.Get("Authorization"); !strings.HasPrefix(got, "Basic ") {
		t.Errorf("PAT auth = %q, want Basic", got)
	}

	req2, _ := http.NewRequest(http.MethodGet, "https://example.test", nil)
	ADOBearer("the-token").authorize(req2)
	if got := req2.Header.Get("Authorization"); got != "Bearer the-token" {
		t.Errorf("app auth = %q, want Bearer the-token", got)
	}

	if !ADOPAT("").empty() || ADOPAT("x").empty() {
		t.Error("empty() wrong")
	}
}
