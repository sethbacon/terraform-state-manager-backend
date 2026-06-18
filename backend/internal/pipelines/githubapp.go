// githubapp.go mints GitHub Actions access tokens from a GitHub App: it signs a
// short-lived app JWT (RS256) with the app's private key, then exchanges it for
// an installation access token. This is the headless, app-owned alternative to a
// personal access token for CI sources whose auth_method is "app" — no user,
// tokens auto-renewed and cached until shortly before expiry.
package pipelines

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// GitHubAppCreds is a GitHub App installation used to mint installation access
// tokens. PrivateKeyPEM is the app's RSA private key in PEM form.
type GitHubAppCreds struct {
	AppID          string
	InstallationID string
	PrivateKeyPEM  string
}

func (c GitHubAppCreds) valid() bool {
	return c.AppID != "" && c.InstallationID != "" && c.PrivateKeyPEM != ""
}

// fingerprint keys the token cache so rotating any credential field invalidates
// the cached token (a different key) without explicit eviction.
func (c GitHubAppCreds) fingerprint() string {
	sum := sha256.Sum256([]byte(c.AppID + "\x00" + c.InstallationID + "\x00" + c.PrivateKeyPEM))
	return string(sum[:])
}

type ghAppCachedToken struct {
	token     string
	expiresAt time.Time
}

var (
	ghAppCacheMu sync.Mutex
	ghAppCache   = map[string]ghAppCachedToken{}
)

// MintGitHubInstallationToken returns a GitHub installation access token for the
// app, minting it (sign app JWT -> exchange for installation token) and caching
// until shortly before expiry. Concurrency-safe.
func MintGitHubInstallationToken(ctx context.Context, creds GitHubAppCreds) (string, error) {
	if !creds.valid() {
		return "", fmt.Errorf("github app credentials require app_id, installation_id, and a private key")
	}
	key := creds.fingerprint()

	ghAppCacheMu.Lock()
	if t, ok := ghAppCache[key]; ok && time.Until(t.expiresAt) > entraTokenRefreshMargin {
		token := t.token
		ghAppCacheMu.Unlock()
		return token, nil
	}
	ghAppCacheMu.Unlock()

	token, expiresAt, err := requestInstallationToken(ctx, creds)
	if err != nil {
		return "", err
	}

	ghAppCacheMu.Lock()
	ghAppCache[key] = ghAppCachedToken{token: token, expiresAt: expiresAt}
	ghAppCacheMu.Unlock()
	return token, nil
}

// requestInstallationToken signs an app JWT and POSTs to the installation
// access-token endpoint.
func requestInstallationToken(ctx context.Context, creds GitHubAppCreds) (string, time.Time, error) {
	signer, err := parseRSAPrivateKey(creds.PrivateKeyPEM)
	if err != nil {
		return "", time.Time{}, err
	}
	appJWT, err := signAppJWT(creds.AppID, signer)
	if err != nil {
		return "", time.Time{}, err
	}

	u := fmt.Sprintf("%s/app/installations/%s/access_tokens",
		githubAPIBaseURL, urlPathSegment(creds.InstallationID))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, nil)
	if err != nil {
		return "", time.Time{}, err
	}
	req.Header.Set("Authorization", "Bearer "+appJWT)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("github installation token request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	if resp.StatusCode != http.StatusCreated {
		return "", time.Time{}, fmt.Errorf("github installation token endpoint returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var out struct {
		Token     string `json:"token"`
		ExpiresAt string `json:"expires_at"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", time.Time{}, fmt.Errorf("github installation token response not JSON: %w", err)
	}
	if out.Token == "" {
		return "", time.Time{}, fmt.Errorf("github installation token response had no token")
	}
	expiresAt, perr := time.Parse(time.RFC3339, out.ExpiresAt)
	if perr != nil {
		expiresAt = time.Now().Add(time.Hour) // GitHub installation tokens last ~1h
	}
	return out.Token, expiresAt, nil
}

// parseRSAPrivateKey accepts a PKCS#1 ("RSA PRIVATE KEY") or PKCS#8 ("PRIVATE
// KEY") PEM and returns the RSA key.
func parseRSAPrivateKey(pemStr string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(pemStr))
	if block == nil {
		return nil, fmt.Errorf("private key is not valid PEM")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("private key is not a supported RSA key (PKCS#1 or PKCS#8): %w", err)
	}
	rsaKey, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("private key is not RSA")
	}
	return rsaKey, nil
}

// ValidRSAPrivateKey reports whether pemStr parses as a supported RSA private
// key — used to validate input before encrypting and storing it.
func ValidRSAPrivateKey(pemStr string) bool {
	_, err := parseRSAPrivateKey(pemStr)
	return err == nil
}

// signAppJWT builds a GitHub App JWT (RS256): header.payload signed with RSA
// PKCS#1 v1.5 over SHA-256. iat is backdated 60s for clock skew; exp is +9m
// (under GitHub's 10-minute maximum).
func signAppJWT(appID string, key *rsa.PrivateKey) (string, error) {
	now := time.Now()
	header := `{"alg":"RS256","typ":"JWT"}`
	claims, err := json.Marshal(map[string]any{
		"iat": now.Add(-60 * time.Second).Unix(),
		"exp": now.Add(9 * time.Minute).Unix(),
		"iss": appID,
	})
	if err != nil {
		return "", err
	}
	signingInput := b64url([]byte(header)) + "." + b64url(claims)
	digest := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("sign app jwt: %w", err)
	}
	return signingInput + "." + b64url(sig), nil
}

func b64url(b []byte) string {
	return base64.RawURLEncoding.EncodeToString(b)
}

// urlPathSegment escapes a single path segment (installation ids are numeric,
// but escape defensively).
func urlPathSegment(s string) string {
	return strings.NewReplacer("/", "%2F", "?", "%3F", "#", "%23").Replace(s)
}

// ResetGitHubAppTokenCacheForTest clears the in-memory token cache between tests.
func ResetGitHubAppTokenCacheForTest() {
	ghAppCacheMu.Lock()
	ghAppCache = map[string]ghAppCachedToken{}
	ghAppCacheMu.Unlock()
}
