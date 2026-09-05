package pipelines

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
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

// fakeTokenCredential is a test double for ADOTokenCredential -- the subset of
// azcore.TokenCredential MintWorkloadIdentityADOToken needs -- so these tests
// never touch a real AKS federated identity. tokens[i]/errs[i] answer the i-th
// call; the last entry repeats for any call past the end.
type fakeTokenCredential struct {
	tokens   []azcore.AccessToken
	errs     []error
	calls    int32
	lastOpts policy.TokenRequestOptions
}

func (f *fakeTokenCredential) GetToken(_ context.Context, opts policy.TokenRequestOptions) (azcore.AccessToken, error) {
	i := int(atomic.AddInt32(&f.calls, 1)) - 1
	f.lastOpts = opts
	if i < len(f.errs) && f.errs[i] != nil {
		return azcore.AccessToken{}, f.errs[i]
	}
	if i < len(f.tokens) {
		return f.tokens[i], nil
	}
	if len(f.tokens) > 0 {
		return f.tokens[len(f.tokens)-1], nil
	}
	return azcore.AccessToken{}, errors.New("fakeTokenCredential: no token configured")
}

func TestMintWorkloadIdentityADOToken_RequiresClientID(t *testing.T) {
	resetEntraCache()
	if _, err := MintWorkloadIdentityADOToken(context.Background(), ""); err == nil {
		t.Error("empty client_id should error")
	}
	if _, err := MintWorkloadIdentityADOToken(context.Background(), "   "); err == nil {
		t.Error("blank (whitespace) client_id should error")
	}
}

// TestMintWorkloadIdentityADOToken_CachesAndRefreshes exercises both halves of
// the cache in one run: a token well inside its TTL is served from cache (no
// second GetToken call), and a token inside the refresh margin is treated as
// due for renewal even though it has not technically expired -- the same
// margin MintEntraADOToken uses, so an in-flight dispatch never races a hard
// expiry.
func TestMintWorkloadIdentityADOToken_CachesAndRefreshes(t *testing.T) {
	resetEntraCache()
	fake := &fakeTokenCredential{tokens: []azcore.AccessToken{
		{Token: "wi-token-1", ExpiresOn: time.Now().Add(30 * time.Second)}, // inside the 60s margin
		{Token: "wi-token-2", ExpiresOn: time.Now().Add(time.Hour)},        // comfortably fresh
	}}
	var gotClientID string
	restore := OverrideWorkloadIdentityCredentialFactoryForTest(func(clientID string) (ADOTokenCredential, error) {
		gotClientID = clientID
		return fake, nil
	})
	defer restore()

	tok, err := MintWorkloadIdentityADOToken(context.Background(), "the-client")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if tok != "wi-token-1" {
		t.Fatalf("token = %q, want wi-token-1", tok)
	}
	if gotClientID != "the-client" {
		t.Errorf("credential factory saw client_id = %q, want the-client", gotClientID)
	}
	if len(fake.lastOpts.Scopes) != 1 || !strings.HasSuffix(fake.lastOpts.Scopes[0], "/.default") {
		t.Errorf("scopes = %v, want the ADO resource id's /.default scope", fake.lastOpts.Scopes)
	}

	// The cached token is inside the refresh margin, so this call re-mints
	// rather than serving it -- proving "refreshes", not just "caches".
	tok2, err := MintWorkloadIdentityADOToken(context.Background(), "the-client")
	if err != nil {
		t.Fatalf("mint (near-expiry): %v", err)
	}
	if tok2 != "wi-token-2" {
		t.Fatalf("token = %q, want wi-token-2 (a fresh mint)", tok2)
	}
	if got := atomic.LoadInt32(&fake.calls); got != 2 {
		t.Fatalf("GetToken called %d times, want 2 (mint, then refresh)", got)
	}

	// wi-token-2 is comfortably fresh, so a THIRD call must be served from
	// cache with no further GetToken call -- proving "caches".
	if _, err := MintWorkloadIdentityADOToken(context.Background(), "the-client"); err != nil {
		t.Fatalf("mint (cached): %v", err)
	}
	if got := atomic.LoadInt32(&fake.calls); got != 2 {
		t.Fatalf("GetToken called %d times after a fresh cache hit, want still 2", got)
	}

	// A different client_id is a different identity, so it must never reuse
	// "the-client"'s cache entry.
	if _, err := MintWorkloadIdentityADOToken(context.Background(), "other-client"); err != nil {
		t.Fatalf("mint (other client): %v", err)
	}
	if got := atomic.LoadInt32(&fake.calls); got != 3 {
		t.Fatalf("GetToken called %d times after a different client_id, want 3", got)
	}
}

func TestMintWorkloadIdentityADOToken_CredentialFactoryError(t *testing.T) {
	resetEntraCache()
	restore := OverrideWorkloadIdentityCredentialFactoryForTest(func(string) (ADOTokenCredential, error) {
		return nil, errors.New("no federated token file")
	})
	defer restore()
	if _, err := MintWorkloadIdentityADOToken(context.Background(), "the-client"); err == nil {
		t.Fatal("expected an error when the credential cannot be constructed")
	}
}

func TestMintWorkloadIdentityADOToken_GetTokenError(t *testing.T) {
	resetEntraCache()
	fake := &fakeTokenCredential{errs: []error{errors.New("AADSTS70021: no matching federated identity record found")}}
	restore := OverrideWorkloadIdentityCredentialFactoryForTest(func(string) (ADOTokenCredential, error) {
		return fake, nil
	})
	defer restore()
	_, err := MintWorkloadIdentityADOToken(context.Background(), "the-client")
	if err == nil {
		t.Fatal("expected an error when GetToken fails")
	}
	if !strings.Contains(err.Error(), "AADSTS70021") {
		t.Errorf("error = %v, want it to surface the federation failure", err)
	}
}

// TestEvictADOTokenCacheKey_RemovesOnlyThatEntry proves the keyed eviction
// PUT /ci-sources/:id relies on (Phase 1b) removes exactly one cache entry,
// leaving an unrelated one untouched -- a full ResetEntraTokenCacheForTest
// would also pass a test that evicted everything, which is not what a
// single-source credential rotation is supposed to do to every OTHER source's
// cached token.
func TestEvictADOTokenCacheKey_RemovesOnlyThatEntry(t *testing.T) {
	resetEntraCache()
	entraCreds := EntraCreds{TenantID: "t", ClientID: "c", ClientSecret: "s"}
	wiCreds := WorkloadIdentityCreds{ClientID: "wi-client"}

	entraCacheMu.Lock()
	entraCache[entraCreds.Fingerprint()] = entraCachedToken{token: "entra-tok", expiresAt: time.Now().Add(time.Hour)}
	entraCache[wiCreds.Fingerprint()] = entraCachedToken{token: "wi-tok", expiresAt: time.Now().Add(time.Hour)}
	entraCacheMu.Unlock()

	EvictADOTokenCacheKey(entraCreds.Fingerprint())

	entraCacheMu.Lock()
	_, entraStillCached := entraCache[entraCreds.Fingerprint()]
	_, wiStillCached := entraCache[wiCreds.Fingerprint()]
	entraCacheMu.Unlock()

	if entraStillCached {
		t.Error("EvictADOTokenCacheKey did not remove the entry it was given")
	}
	if !wiStillCached {
		t.Error("EvictADOTokenCacheKey removed an unrelated entry -- it must evict only the given key")
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
