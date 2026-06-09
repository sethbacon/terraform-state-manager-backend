// Package federation provides access tokens for outbound calls to Azure DevOps
// using Workload Identity Federation (WIF). Instead of a stored secret, TSM
// reads a projected Kubernetes service-account OIDC token from disk and presents
// it to Entra ID's (AAD) token endpoint as a client_assertion, receiving an
// access token scoped to Azure DevOps in return.
//
// The exchange follows the OAuth 2.0 client-credentials grant with a JWT bearer
// client assertion (RFC 7523):
//
//	POST {tenant token endpoint}
//	  grant_type=client_credentials
//	  client_id={Entra app/client id}
//	  scope={ado resource}/.default
//	  client_assertion_type=urn:ietf:params:oauth:client-assertion-type:jwt-bearer
//	  client_assertion={projected SA OIDC token read from TokenFilePath}
//
// This is the OUTBOUND counterpart to the inbound drift-ingest validator
// (internal/auth/driftingest): there, TSM verifies an OIDC token presented by a
// CI pipeline; here, TSM presents its own projected OIDC token to obtain a
// bearer for calling Azure DevOps.
//
// Mock-first: there is no AKS or Entra tenant deployed yet. The real
// FederatedTokenProvider is configurable to point at any token endpoint, so unit
// tests drive it against an httptest server (no live calls). A StaticTokenProvider
// is provided for callers and tests that just need a fixed bearer.
package federation

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// clientAssertionType is the fixed assertion type for the JWT-bearer
// client-credentials flow (RFC 7523 / Entra workload-identity federation).
// #nosec G101 -- this is the public OAuth assertion-type URN, not a credential.
const clientAssertionType = "urn:ietf:params:oauth:client-assertion-type:jwt-bearer"

// expiryLeeway is subtracted from the token's reported lifetime so the cached
// token is refreshed slightly before it actually expires, avoiding races with
// an in-flight ADO request that would otherwise be rejected as expired.
const expiryLeeway = 60 * time.Second

// TokenProvider supplies a bearer access token for outbound Azure DevOps REST
// calls. Implementations may return a static token (tests) or perform the WIF
// exchange and cache the result (production). Token must be safe for concurrent
// use.
type TokenProvider interface {
	// Token returns a currently-valid bearer access token, refreshing it if
	// necessary. The returned string is the raw token value (no "Bearer " prefix).
	Token(ctx context.Context) (string, error)
}

// StaticTokenProvider is a TokenProvider that always returns the same token. It
// exists for tests and for early wiring where no live federation is available.
type StaticTokenProvider struct {
	Value string
}

// Token returns the static token value. It never errors.
func (s StaticTokenProvider) Token(_ context.Context) (string, error) {
	return s.Value, nil
}

// Config configures the federated-token exchange. All fields are required for a
// live exchange; the zero value is rejected by NewProvider. The values map onto
// the TSM_ADO_WIF_* configuration keys.
type Config struct {
	// TokenEndpoint is the full Entra (AAD) token endpoint for the tenant, e.g.
	// https://login.microsoftonline.com/{tenant}/oauth2/v2.0/token. Tests point
	// this at an httptest server.
	TokenEndpoint string
	// ClientID is the Entra application (client) id whose federated credential
	// trusts the projected service-account issuer/subject.
	ClientID string
	// Scope is the OAuth scope requested for Azure DevOps, typically the ADO
	// resource id suffixed with "/.default", e.g.
	// "499b84ac-1321-427f-aa17-267ca6975798/.default".
	Scope string
	// TokenFilePath is the filesystem path of the projected service-account OIDC
	// token (the client assertion). In AKS this is the projected SA token volume.
	TokenFilePath string
}

func (c Config) validate() error {
	if c.TokenEndpoint == "" {
		return fmt.Errorf("federation: TokenEndpoint is required")
	}
	if c.ClientID == "" {
		return fmt.Errorf("federation: ClientID is required")
	}
	if c.Scope == "" {
		return fmt.Errorf("federation: Scope is required")
	}
	if c.TokenFilePath == "" {
		return fmt.Errorf("federation: TokenFilePath is required")
	}
	return nil
}

// FederatedTokenProvider exchanges a projected service-account OIDC token for an
// Azure DevOps access token via the Entra token endpoint and caches the result
// until shortly before it expires. It is safe for concurrent use.
type FederatedTokenProvider struct {
	cfg        Config
	httpClient *http.Client
	now        func() time.Time // injectable clock for tests

	mu          sync.Mutex
	cachedToken string
	cachedUntil time.Time
}

// NewProvider builds a FederatedTokenProvider from cfg. The provided httpClient
// is used for the token exchange; pass nil to use a default client with a short
// timeout. cfg is validated and an error is returned if any required field is
// missing.
func NewProvider(cfg Config, httpClient *http.Client) (*FederatedTokenProvider, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	return &FederatedTokenProvider{
		cfg:        cfg,
		httpClient: httpClient,
		now:        time.Now,
	}, nil
}

// tokenResponse is the subset of the Entra token-endpoint JSON response we use.
type tokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int64  `json:"expires_in"` // seconds until expiry
}

// tokenErrorResponse is the OAuth error shape returned on a non-2xx exchange.
type tokenErrorResponse struct {
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

// Token returns a cached token if it is still valid, otherwise performs a fresh
// WIF exchange. The exchange reads the projected assertion afresh each time so a
// rotated service-account token is always picked up.
func (p *FederatedTokenProvider) Token(ctx context.Context) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.cachedToken != "" && p.now().Before(p.cachedUntil) {
		return p.cachedToken, nil
	}

	tok, ttl, err := p.exchange(ctx)
	if err != nil {
		return "", err
	}

	p.cachedToken = tok
	p.cachedUntil = p.now().Add(ttl - expiryLeeway)
	return tok, nil
}

// exchange reads the projected assertion and performs the client-assertion
// token exchange, returning the access token and its lifetime.
func (p *FederatedTokenProvider) exchange(ctx context.Context) (string, time.Duration, error) {
	assertion, err := p.readAssertion()
	if err != nil {
		return "", 0, err
	}

	form := url.Values{}
	form.Set("grant_type", "client_credentials")
	form.Set("client_id", p.cfg.ClientID)
	form.Set("scope", p.cfg.Scope)
	form.Set("client_assertion_type", clientAssertionType)
	form.Set("client_assertion", assertion)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.cfg.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", 0, fmt.Errorf("federation: building token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("federation: token exchange request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", 0, fmt.Errorf("federation: reading token response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var oauthErr tokenErrorResponse
		_ = json.Unmarshal(body, &oauthErr)
		if oauthErr.Error != "" {
			return "", 0, fmt.Errorf("federation: token exchange failed (%d): %s: %s", resp.StatusCode, oauthErr.Error, oauthErr.ErrorDescription)
		}
		return "", 0, fmt.Errorf("federation: token exchange failed (%d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var tr tokenResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return "", 0, fmt.Errorf("federation: decoding token response: %w", err)
	}
	if tr.AccessToken == "" {
		return "", 0, fmt.Errorf("federation: token response contained no access_token")
	}

	ttl := time.Duration(tr.ExpiresIn) * time.Second
	if ttl <= 0 {
		// Defensive: if the endpoint omits expires_in, treat the token as
		// short-lived so we re-exchange on the next call rather than caching a
		// token of unknown lifetime.
		ttl = expiryLeeway + time.Second
	}
	return tr.AccessToken, ttl, nil
}

// readAssertion reads the projected service-account OIDC token from disk and
// trims surrounding whitespace (projected SA token files commonly end in a
// newline).
func (p *FederatedTokenProvider) readAssertion() (string, error) {
	// #nosec G304 -- TokenFilePath is an operator-configured path to the
	// projected service-account token volume, not attacker-controlled input.
	data, err := os.ReadFile(p.cfg.TokenFilePath)
	if err != nil {
		return "", fmt.Errorf("federation: reading projected token file %q: %w", p.cfg.TokenFilePath, err)
	}
	assertion := strings.TrimSpace(string(data))
	if assertion == "" {
		return "", fmt.Errorf("federation: projected token file %q is empty", p.cfg.TokenFilePath)
	}
	return assertion, nil
}
