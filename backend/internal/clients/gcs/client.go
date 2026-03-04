// Package gcs implements a Terraform state file scanner for Google Cloud
// Storage.
//
// To avoid pulling in the full GCS client library, this client uses the
// Google Cloud Storage JSON API v1 directly via net/http.  Service-account
// authentication is supported through a simplified JWT-based OAuth2 flow.
package gcs

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
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/terraform-state-manager/terraform-state-manager/internal/clients/azure"
)

// Config holds the parameters needed to connect to a GCS bucket.
type Config struct {
	// Bucket is the GCS bucket name.
	Bucket string `json:"bucket"`
	// Prefix filters objects whose names start with this value.
	Prefix string `json:"prefix,omitempty"`
	// CredentialsJSON is the raw JSON content of a service-account key file.
	// If empty, the client assumes Application Default Credentials or a
	// metadata-based token is available.
	CredentialsJSON string `json:"credentials_json,omitempty"`
}

// Client communicates with Google Cloud Storage through the JSON API.
type Client struct {
	config     Config
	httpClient *http.Client

	// Cached OAuth2 access token and its expiry.
	accessToken string
	tokenExpiry time.Time
}

// serviceAccountKey represents the relevant fields of a GCP service-account
// key JSON file.
type serviceAccountKey struct {
	Type        string `json:"type"`
	ClientEmail string `json:"client_email"`
	PrivateKey  string `json:"private_key"`
	TokenURI    string `json:"token_uri"`
}

// NewClient validates the configuration and returns a ready-to-use Client.
func NewClient(cfg Config) (*Client, error) {
	if cfg.Bucket == "" {
		return nil, fmt.Errorf("gcs: bucket is required")
	}

	return &Client{
		config: cfg,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}, nil
}

// apiBase is the root of the GCS JSON API.
const apiBase = "https://storage.googleapis.com/storage/v1"

// TestConnection verifies that the configured bucket is reachable by fetching
// its metadata.
func (c *Client) TestConnection(ctx context.Context) error {
	reqURL := fmt.Sprintf("%s/b/%s", apiBase, url.PathEscape(c.config.Bucket))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return fmt.Errorf("gcs: failed to create request: %w", err)
	}

	if err := c.authorize(req); err != nil {
		return fmt.Errorf("gcs: authorization failed: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("gcs: connection test failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("gcs: connection test returned status %d: %s", resp.StatusCode, string(body))
	}

	return nil
}

// gcsListResponse mirrors the relevant fields of the GCS Objects: list JSON
// response.
type gcsListResponse struct {
	Items         []gcsObject `json:"items"`
	NextPageToken string      `json:"nextPageToken"`
}

type gcsObject struct {
	Name    string `json:"name"`
	Updated string `json:"updated"`
	Size    string `json:"size"`
}

// ListStateFiles enumerates all objects in the bucket whose names start with
// the configured prefix and end with ".tfstate".
func (c *Client) ListStateFiles(ctx context.Context) ([]azure.StateFileRef, error) {
	var refs []azure.StateFileRef
	pageToken := ""

	for {
		params := url.Values{}
		if c.config.Prefix != "" {
			params.Set("prefix", c.config.Prefix)
		}
		if pageToken != "" {
			params.Set("pageToken", pageToken)
		}

		reqURL := fmt.Sprintf("%s/b/%s/o?%s", apiBase, url.PathEscape(c.config.Bucket), params.Encode())
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
		if err != nil {
			return nil, fmt.Errorf("gcs: failed to create list request: %w", err)
		}

		if err := c.authorize(req); err != nil {
			return nil, fmt.Errorf("gcs: authorization failed: %w", err)
		}

		resp, err := c.httpClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("gcs: list objects request failed: %w", err)
		}

		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("gcs: failed to read list response: %w", err)
		}

		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("gcs: list objects returned status %d: %s", resp.StatusCode, string(body))
		}

		var result gcsListResponse
		if err := json.Unmarshal(body, &result); err != nil {
			return nil, fmt.Errorf("gcs: failed to parse list response: %w", err)
		}

		for _, obj := range result.Items {
			if !strings.HasSuffix(obj.Name, ".tfstate") {
				continue
			}
			lastMod, _ := time.Parse(time.RFC3339, obj.Updated)
			var size int64
			if _, err := fmt.Sscanf(obj.Size, "%d", &size); err != nil {
				size = 0
			}
			refs = append(refs, azure.StateFileRef{
				Key:          obj.Name,
				LastModified: lastMod,
				Size:         size,
			})
		}

		if result.NextPageToken == "" {
			break
		}
		pageToken = result.NextPageToken
	}

	sort.Slice(refs, func(i, j int) bool {
		return refs[i].Key < refs[j].Key
	})

	return refs, nil
}

// DownloadState retrieves the raw content of the GCS object referenced by ref.
func (c *Client) DownloadState(ctx context.Context, ref azure.StateFileRef) ([]byte, error) {
	reqURL := fmt.Sprintf("%s/b/%s/o/%s?alt=media",
		apiBase,
		url.PathEscape(c.config.Bucket),
		url.PathEscape(ref.Key),
	)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("gcs: failed to create download request: %w", err)
	}

	if err := c.authorize(req); err != nil {
		return nil, fmt.Errorf("gcs: authorization failed: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("gcs: download request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("gcs: download returned status %d: %s", resp.StatusCode, string(body))
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("gcs: failed to read object content: %w", err)
	}

	return data, nil
}

// ---------------------------------------------------------------------------
// OAuth2 / JWT authentication for service-account credentials
// ---------------------------------------------------------------------------

// authorize sets the Authorization header on the request.  When
// CredentialsJSON is provided it obtains a short-lived access token via the
// OAuth2 JWT assertion flow.  When no credentials are configured it attempts
// to use the GCE metadata server.
func (c *Client) authorize(req *http.Request) error {
	if c.config.CredentialsJSON == "" {
		// Attempt metadata-server token (works on GCE/GKE/Cloud Run).
		return c.authorizeFromMetadata(req)
	}

	// Refresh the token if it is about to expire.
	if c.accessToken == "" || time.Now().After(c.tokenExpiry.Add(-1*time.Minute)) {
		if err := c.refreshToken(); err != nil {
			return err
		}
	}

	req.Header.Set("Authorization", "Bearer "+c.accessToken)
	return nil
}

// authorizeFromMetadata fetches an access token from the GCE metadata server.
func (c *Client) authorizeFromMetadata(req *http.Request) error {
	metaReq, err := http.NewRequestWithContext(req.Context(), http.MethodGet,
		"http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/token", nil)
	if err != nil {
		return fmt.Errorf("gcs: failed to create metadata request: %w", err)
	}
	metaReq.Header.Set("Metadata-Flavor", "Google")

	resp, err := c.httpClient.Do(metaReq)
	if err != nil {
		return fmt.Errorf("gcs: metadata token request failed (are you running on GCP?): %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("gcs: metadata token returned status %d", resp.StatusCode)
	}

	var tokenResp struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
		return fmt.Errorf("gcs: failed to decode metadata token: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+tokenResp.AccessToken)
	return nil
}

// refreshToken obtains a new OAuth2 access token by creating a signed JWT
// and exchanging it at the token endpoint.
func (c *Client) refreshToken() error {
	var sa serviceAccountKey
	if err := json.Unmarshal([]byte(c.config.CredentialsJSON), &sa); err != nil {
		return fmt.Errorf("gcs: failed to parse credentials JSON: %w", err)
	}

	if sa.TokenURI == "" {
		sa.TokenURI = "https://oauth2.googleapis.com/token"
	}

	now := time.Now().UTC()
	claims := map[string]interface{}{
		"iss":   sa.ClientEmail,
		"scope": "https://www.googleapis.com/auth/devstorage.read_only",
		"aud":   sa.TokenURI,
		"iat":   now.Unix(),
		"exp":   now.Add(1 * time.Hour).Unix(),
	}

	jwt, err := c.buildJWT(claims, sa.PrivateKey)
	if err != nil {
		return fmt.Errorf("gcs: failed to build JWT: %w", err)
	}

	data := url.Values{
		"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"},
		"assertion":  {jwt},
	}

	resp, err := c.httpClient.PostForm(sa.TokenURI, data)
	if err != nil {
		return fmt.Errorf("gcs: token exchange failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("gcs: token exchange returned status %d: %s", resp.StatusCode, string(body))
	}

	var tokenResp struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
		return fmt.Errorf("gcs: failed to decode token response: %w", err)
	}

	c.accessToken = tokenResp.AccessToken
	c.tokenExpiry = now.Add(time.Duration(tokenResp.ExpiresIn) * time.Second)

	return nil
}

// buildJWT creates an RS256-signed JWT from the given claims and PEM-encoded
// private key.
func (c *Client) buildJWT(claims map[string]interface{}, privateKeyPEM string) (string, error) {
	header := base64URLEncode([]byte(`{"alg":"RS256","typ":"JWT"}`))

	claimsJSON, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("failed to marshal claims: %w", err)
	}
	payload := base64URLEncode(claimsJSON)

	signingInput := header + "." + payload

	block, _ := pem.Decode([]byte(privateKeyPEM))
	if block == nil {
		return "", fmt.Errorf("failed to decode PEM block")
	}

	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return "", fmt.Errorf("failed to parse private key: %w", err)
	}

	rsaKey, ok := key.(*rsa.PrivateKey)
	if !ok {
		return "", fmt.Errorf("private key is not RSA")
	}

	hashed := sha256.Sum256([]byte(signingInput))
	signature, err := rsa.SignPKCS1v15(rand.Reader, rsaKey, crypto.SHA256, hashed[:])
	if err != nil {
		return "", fmt.Errorf("failed to sign JWT: %w", err)
	}

	return signingInput + "." + base64URLEncode(signature), nil
}

// base64URLEncode encodes data using the URL-safe base64 alphabet without
// padding.
func base64URLEncode(data []byte) string {
	return base64.RawURLEncoding.EncodeToString(data)
}
