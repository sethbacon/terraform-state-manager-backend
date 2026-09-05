// entra.go mints Azure DevOps access tokens from a Microsoft Entra app
// registration using the OAuth 2.0 client-credentials grant. This is the
// headless, app-owned alternative to a personal access token for CI sources
// whose auth_method is "app": no user, no refresh token (the grant simply
// re-mints), tokens auto-renewed and cached until shortly before expiry.
package pipelines

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
)

// azureDevOpsResourceID is the fixed Microsoft Entra resource (application) id
// for Azure DevOps. Requesting "<id>/.default" yields a token carrying whatever
// ADO permissions the app registration has been granted.
const azureDevOpsResourceID = "499b84ac-1321-427f-aa17-267ca6975798"

// entraLoginBaseURL is the Microsoft Entra (Azure AD) login host. A package var
// so tests can point it at an httptest server.
var entraLoginBaseURL = "https://login.microsoftonline.com"

// OverrideEntraLoginURLForTest points the Entra token host at a test server and
// returns a restore func. For handler tests in dependent packages; not safe for
// concurrent use with real traffic.
func OverrideEntraLoginURLForTest(u string) (restore func()) {
	old := entraLoginBaseURL
	if u != "" {
		entraLoginBaseURL = u
	}
	return func() { entraLoginBaseURL = old }
}

// ResetEntraTokenCacheForTest clears the in-memory token cache between tests.
func ResetEntraTokenCacheForTest() {
	entraCacheMu.Lock()
	entraCache = map[string]entraCachedToken{}
	entraCacheMu.Unlock()
}

// EntraCreds is a Microsoft Entra app registration used to mint Azure DevOps
// access tokens via client-credentials.
type EntraCreds struct {
	TenantID     string
	ClientID     string
	ClientSecret string
}

func (c EntraCreds) valid() bool {
	return c.TenantID != "" && c.ClientID != "" && c.ClientSecret != ""
}

// Fingerprint keys the token cache so rotating any credential field naturally
// invalidates the cached token (a different key) without explicit eviction.
// Exported so a credential UPDATE (PUT /ci-sources/:id) can compute the OLD
// row's cache key and evict it explicitly via EvictADOTokenCacheKey, rather
// than relying solely on this implicit "a different key" property.
func (c EntraCreds) Fingerprint() string {
	sum := sha256.Sum256([]byte("app\x00" + c.TenantID + "\x00" + c.ClientID + "\x00" + c.ClientSecret))
	return hex.EncodeToString(sum[:])
}

type entraCachedToken struct {
	token     string
	expiresAt time.Time
}

var (
	entraCacheMu sync.Mutex
	entraCache   = map[string]entraCachedToken{}
)

// entraTokenRefreshMargin re-mints this long before the cached token's expiry so
// an in-flight dispatch never races a hard expiry.
const entraTokenRefreshMargin = 60 * time.Second

// MintEntraADOToken returns a bearer access token for Azure DevOps, minting via
// the Entra client-credentials grant and caching it until shortly before expiry.
// Concurrency-safe.
func MintEntraADOToken(ctx context.Context, creds EntraCreds) (string, error) {
	if !creds.valid() {
		return "", fmt.Errorf("entra app credentials require tenant_id, client_id, and client_secret")
	}
	key := creds.Fingerprint()

	entraCacheMu.Lock()
	if t, ok := entraCache[key]; ok && time.Until(t.expiresAt) > entraTokenRefreshMargin {
		token := t.token
		entraCacheMu.Unlock()
		return token, nil
	}
	entraCacheMu.Unlock()

	token, expiresAt, err := requestEntraToken(ctx, creds)
	if err != nil {
		return "", err
	}

	entraCacheMu.Lock()
	entraCache[key] = entraCachedToken{token: token, expiresAt: expiresAt}
	entraCacheMu.Unlock()
	return token, nil
}

// EvictADOTokenCacheKey removes a single cached token, keyed by the same
// Fingerprint EntraCreds and WorkloadIdentityCreds compute.
//
// ResetEntraTokenCacheForTest clears everything, for test isolation between
// unrelated cases. This clears exactly one row, for a real credential
// rotation: PUT /ci-sources/:id calls it with the fingerprint of the
// credential a source USED TO carry, so a token minted under that credential
// can never be served again once the row no longer has it. Rotating a
// credential to a genuinely DIFFERENT value already gets this for free --
// Fingerprint changes with it, so the old entry is simply never looked up
// again -- but a route whose whole job is "replace this row's credential"
// should not depend on that being true forever; an explicit evict makes it an
// invariant of the route instead of a property that happens to fall out of
// how the cache is keyed today.
func EvictADOTokenCacheKey(key string) {
	if key == "" {
		return
	}
	entraCacheMu.Lock()
	delete(entraCache, key)
	entraCacheMu.Unlock()
}

// WorkloadIdentityCreds identifies an AKS Workload Identity used to mint Azure
// DevOps access tokens: unlike EntraCreds there is no secret material at all --
// just the federated user-assigned managed identity's client id. TenantID and
// the path to the projected service-account token are resolved from the pod's
// own environment (AZURE_TENANT_ID / AZURE_FEDERATED_TOKEN_FILE, set by the
// AKS workload-identity webhook), not stored on the CI source.
type WorkloadIdentityCreds struct {
	ClientID string
}

func (c WorkloadIdentityCreds) valid() bool { return strings.TrimSpace(c.ClientID) != "" }

// Fingerprint keys the token cache. Namespaced ("workload_identity\x00") so a
// workload-identity client id can never collide with an EntraCreds fingerprint
// in the shared cache map, even though the two share nothing else -- an
// EntraCreds always folds in a tenant id and a secret, so an accidental
// collision would need those to be empty, which EntraCreds.valid() already
// refuses to mint against; the namespace makes that a non-issue regardless.
func (c WorkloadIdentityCreds) Fingerprint() string {
	sum := sha256.Sum256([]byte("workload_identity\x00" + c.ClientID))
	return hex.EncodeToString(sum[:])
}

// ADOTokenCredential is the subset of azcore.TokenCredential
// MintWorkloadIdentityADOToken needs, satisfied by
// *azidentity.WorkloadIdentityCredential and by a fake in tests.
type ADOTokenCredential interface {
	GetToken(ctx context.Context, opts policy.TokenRequestOptions) (azcore.AccessToken, error)
}

// workloadIdentityCredentialFactory builds the azidentity credential for a
// given managed identity client id. A package var so tests can substitute a
// fake TokenCredential without a real AKS federated identity; production never
// overrides it.
var workloadIdentityCredentialFactory = func(clientID string) (ADOTokenCredential, error) {
	return azidentity.NewWorkloadIdentityCredential(&azidentity.WorkloadIdentityCredentialOptions{
		ClientID: clientID,
	})
}

// OverrideWorkloadIdentityCredentialFactoryForTest points
// MintWorkloadIdentityADOToken at a fake credential and returns a restore
// func. Not safe for concurrent use with real traffic.
func OverrideWorkloadIdentityCredentialFactoryForTest(f func(clientID string) (ADOTokenCredential, error)) (restore func()) {
	old := workloadIdentityCredentialFactory
	workloadIdentityCredentialFactory = f
	return func() { workloadIdentityCredentialFactory = old }
}

// MintWorkloadIdentityADOToken returns a bearer access token for Azure DevOps,
// minting via AKS Workload Identity's federated-token exchange and caching it
// until shortly before expiry -- same cache, same refresh margin, same
// concurrency guarantee as MintEntraADOToken, just a different credential
// source (no client secret ever exists for this method).
func MintWorkloadIdentityADOToken(ctx context.Context, clientID string) (string, error) {
	creds := WorkloadIdentityCreds{ClientID: strings.TrimSpace(clientID)}
	if !creds.valid() {
		return "", fmt.Errorf("workload identity credentials require a client_id")
	}
	key := creds.Fingerprint()

	entraCacheMu.Lock()
	if t, ok := entraCache[key]; ok && time.Until(t.expiresAt) > entraTokenRefreshMargin {
		token := t.token
		entraCacheMu.Unlock()
		return token, nil
	}
	entraCacheMu.Unlock()

	cred, err := workloadIdentityCredentialFactory(creds.ClientID)
	if err != nil {
		return "", fmt.Errorf("build workload identity credential: %w", err)
	}
	tok, err := cred.GetToken(ctx, policy.TokenRequestOptions{Scopes: []string{azureDevOpsResourceID + "/.default"}})
	if err != nil {
		return "", fmt.Errorf("mint workload identity ADO token: %w", err)
	}

	entraCacheMu.Lock()
	entraCache[key] = entraCachedToken{token: tok.Token, expiresAt: tok.ExpiresOn}
	entraCacheMu.Unlock()
	return tok.Token, nil
}

// requestEntraToken performs the client-credentials POST against the tenant's
// v2.0 token endpoint.
func requestEntraToken(ctx context.Context, creds EntraCreds) (string, time.Time, error) {
	form := url.Values{}
	form.Set("grant_type", "client_credentials")
	form.Set("client_id", creds.ClientID)
	form.Set("client_secret", creds.ClientSecret)
	form.Set("scope", azureDevOpsResourceID+"/.default")

	endpoint := fmt.Sprintf("%s/%s/oauth2/v2.0/token",
		strings.TrimRight(entraLoginBaseURL, "/"), url.PathEscape(creds.TenantID))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", time.Time{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("entra token request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	if resp.StatusCode != http.StatusOK {
		// Entra returns AADSTS error codes in the body; surface a trimmed form.
		return "", time.Time{}, fmt.Errorf("entra token endpoint returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", time.Time{}, fmt.Errorf("entra token response not JSON: %w", err)
	}
	if out.AccessToken == "" {
		return "", time.Time{}, fmt.Errorf("entra token response had no access_token")
	}
	ttl := time.Duration(out.ExpiresIn) * time.Second
	if ttl <= 0 {
		ttl = time.Hour // Entra default; defensive when expires_in is absent
	}
	return out.AccessToken, time.Now().Add(ttl), nil
}
