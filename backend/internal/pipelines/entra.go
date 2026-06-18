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

// fingerprint keys the token cache so rotating any credential field naturally
// invalidates the cached token (a different key) without explicit eviction.
func (c EntraCreds) fingerprint() string {
	sum := sha256.Sum256([]byte(c.TenantID + "\x00" + c.ClientID + "\x00" + c.ClientSecret))
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
	key := creds.fingerprint()

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
