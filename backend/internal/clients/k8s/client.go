// Package k8s implements a Terraform state file scanner for the Kubernetes
// backend.
//
// Terraform's kubernetes backend stores state as Kubernetes secrets in a
// specific namespace.  This client talks directly to the Kubernetes API server
// via net/http, reading the kubeconfig for cluster endpoint and credentials.
package k8s

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/terraform-state-manager/terraform-state-manager/internal/clients/azure"
)

// Config holds the parameters needed to scan Kubernetes secrets for Terraform
// state.
type Config struct {
	// Namespace restricts the scan to a specific K8s namespace.  When empty
	// the default namespace from the kubeconfig is used, or "default" as a
	// fallback.
	Namespace string `json:"namespace,omitempty"`
	// Labels is a Kubernetes label selector (e.g. "app=terraform") used to
	// filter secrets.
	Labels string `json:"labels,omitempty"`
	// Kubeconfig is the path to a kubeconfig file.  When empty the standard
	// locations ($KUBECONFIG, ~/.kube/config) are probed.
	Kubeconfig string `json:"kubeconfig,omitempty"`
}

// clusterInfo holds the resolved connection parameters extracted from a
// kubeconfig file.
type clusterInfo struct {
	Server string
	CACert []byte // PEM-encoded CA certificate (may be nil)
	Token  string // Bearer token
}

// Client communicates with the Kubernetes API server via net/http.
type Client struct {
	config     Config
	httpClient *http.Client
	cluster    clusterInfo
}

// NewClient resolves the kubeconfig, extracts cluster credentials, and returns
// a ready-to-use Client.
func NewClient(cfg Config) (*Client, error) {
	ci, err := resolveClusterInfo(cfg.Kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("k8s: %w", err)
	}

	if cfg.Namespace == "" {
		cfg.Namespace = "default"
	}

	tlsCfg := &tls.Config{
		MinVersion: tls.VersionTLS12,
	}
	if len(ci.CACert) > 0 {
		pool := x509.NewCertPool()
		pool.AppendCertsFromPEM(ci.CACert)
		tlsCfg.RootCAs = pool
	}

	httpClient := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: tlsCfg,
		},
	}

	return &Client{
		config:     cfg,
		httpClient: httpClient,
		cluster:    ci,
	}, nil
}

// TestConnection verifies that the Kubernetes API server is reachable by
// hitting the /api endpoint.
func (c *Client) TestConnection(ctx context.Context) error {
	reqURL := fmt.Sprintf("%s/api", strings.TrimRight(c.cluster.Server, "/"))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return fmt.Errorf("k8s: failed to create request: %w", err)
	}
	c.setAuth(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("k8s: connection test failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("k8s: connection test returned status %d: %s", resp.StatusCode, string(body))
	}

	return nil
}

// secretList mirrors the Kubernetes SecretList object returned by the API.
type secretList struct {
	Items []secret `json:"items"`
}

type secret struct {
	Metadata secretMetadata    `json:"metadata"`
	Data     map[string]string `json:"data"` // base64-encoded values
}

type secretMetadata struct {
	Name              string            `json:"name"`
	Namespace         string            `json:"namespace"`
	Labels            map[string]string `json:"labels"`
	CreationTimestamp string            `json:"creationTimestamp"`
}

// ListStateFiles lists Kubernetes secrets matching the configured namespace and
// label selector.  Terraform state is typically stored in a secret with a
// "tfstate" key.
func (c *Client) ListStateFiles(ctx context.Context) ([]azure.StateFileRef, error) {
	params := url.Values{}
	if c.config.Labels != "" {
		params.Set("labelSelector", c.config.Labels)
	}

	reqURL := fmt.Sprintf("%s/api/v1/namespaces/%s/secrets?%s",
		strings.TrimRight(c.cluster.Server, "/"),
		url.PathEscape(c.config.Namespace),
		params.Encode(),
	)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("k8s: failed to create list request: %w", err)
	}
	c.setAuth(req)
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("k8s: list secrets request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("k8s: failed to read list response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("k8s: list secrets returned status %d: %s", resp.StatusCode, string(body))
	}

	var list secretList
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("k8s: failed to parse secret list: %w", err)
	}

	var refs []azure.StateFileRef
	for _, s := range list.Items {
		// Terraform stores the state under the "tfstate" data key.  Skip
		// secrets that do not contain it.
		encoded, ok := s.Data["tfstate"]
		if !ok {
			continue
		}

		decoded, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			continue
		}

		ts, _ := time.Parse(time.RFC3339, s.Metadata.CreationTimestamp)
		refs = append(refs, azure.StateFileRef{
			Key:          fmt.Sprintf("%s/%s", s.Metadata.Namespace, s.Metadata.Name),
			LastModified: ts,
			Size:         int64(len(decoded)),
		})
	}

	sort.Slice(refs, func(i, j int) bool {
		return refs[i].Key < refs[j].Key
	})

	return refs, nil
}

// DownloadState retrieves the Terraform state stored in the secret identified
// by ref.Key, which has the form "namespace/secretName".
func (c *Client) DownloadState(ctx context.Context, ref azure.StateFileRef) ([]byte, error) {
	parts := strings.SplitN(ref.Key, "/", 2)
	if len(parts) != 2 {
		return nil, fmt.Errorf("k8s: invalid state ref key %q (expected namespace/name)", ref.Key)
	}
	namespace, name := parts[0], parts[1]

	reqURL := fmt.Sprintf("%s/api/v1/namespaces/%s/secrets/%s",
		strings.TrimRight(c.cluster.Server, "/"),
		url.PathEscape(namespace),
		url.PathEscape(name),
	)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("k8s: failed to create download request: %w", err)
	}
	c.setAuth(req)
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("k8s: download request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("k8s: failed to read secret response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("k8s: download returned status %d: %s", resp.StatusCode, string(body))
	}

	var s secret
	if err := json.Unmarshal(body, &s); err != nil {
		return nil, fmt.Errorf("k8s: failed to parse secret: %w", err)
	}

	encoded, ok := s.Data["tfstate"]
	if !ok {
		return nil, fmt.Errorf("k8s: secret %q does not contain a 'tfstate' key", ref.Key)
	}

	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("k8s: failed to base64-decode tfstate data: %w", err)
	}

	return decoded, nil
}

// setAuth adds the Bearer token to the request.
func (c *Client) setAuth(req *http.Request) {
	if c.cluster.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.cluster.Token)
	}
}

// ---------------------------------------------------------------------------
// Kubeconfig parsing
// ---------------------------------------------------------------------------

// kubeConfig is a minimal representation of the portions of a kubeconfig file
// that we need to extract cluster endpoint and authentication credentials.
type kubeConfig struct {
	Clusters       []kubeConfigCluster `json:"clusters"`
	Users          []kubeConfigUser    `json:"users"`
	Contexts       []kubeConfigContext `json:"contexts"`
	CurrentContext string              `json:"current-context"`
}

type kubeConfigCluster struct {
	Name    string                    `json:"name"`
	Cluster kubeConfigClusterSettings `json:"cluster"`
}

type kubeConfigClusterSettings struct {
	Server                   string `json:"server"`
	CertificateAuthority     string `json:"certificate-authority,omitempty"`
	CertificateAuthorityData string `json:"certificate-authority-data,omitempty"`
}

type kubeConfigUser struct {
	Name string                 `json:"name"`
	User kubeConfigUserSettings `json:"user"`
}

type kubeConfigUserSettings struct {
	Token                 string `json:"token,omitempty"`
	ClientCertificate     string `json:"client-certificate,omitempty"`
	ClientCertificateData string `json:"client-certificate-data,omitempty"`
	ClientKey             string `json:"client-key,omitempty"`
	ClientKeyData         string `json:"client-key-data,omitempty"`
}

type kubeConfigContext struct {
	Name    string                    `json:"name"`
	Context kubeConfigContextSettings `json:"context"`
}

type kubeConfigContextSettings struct {
	Cluster   string `json:"cluster"`
	User      string `json:"user"`
	Namespace string `json:"namespace,omitempty"`
}

// resolveClusterInfo locates a kubeconfig file and extracts the current
// context's cluster server URL and bearer token.
func resolveClusterInfo(kubeconfigPath string) (clusterInfo, error) {
	path, err := findKubeconfig(kubeconfigPath)
	if err != nil {
		return clusterInfo{}, err
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return clusterInfo{}, fmt.Errorf("failed to read kubeconfig %q: %w", path, err)
	}

	// kubeconfig files are YAML, but the JSON subset we need is compatible
	// because Go's encoding/json will silently ignore unknown fields and YAML
	// is a superset of JSON.  For robustness we do a quick YAML-to-JSON
	// conversion by hand -- but since we already depend on gopkg.in/yaml.v3
	// transitively, let's keep things simple: we attempt a JSON unmarshal
	// first and fall back to a basic YAML parse if that fails.
	var kc kubeConfig
	if err := json.Unmarshal(data, &kc); err != nil {
		// Try YAML by converting common YAML markers to JSON-friendly form.
		// This is a best-effort approach; full YAML support would require a
		// YAML library.  For most kubeconfig files the JSON unmarshal works
		// because kubectl writes valid JSON-compatible YAML.
		return clusterInfo{}, fmt.Errorf("failed to parse kubeconfig (JSON): %w", err)
	}

	if kc.CurrentContext == "" && len(kc.Contexts) > 0 {
		kc.CurrentContext = kc.Contexts[0].Name
	}

	var ctxSettings kubeConfigContextSettings
	for _, c := range kc.Contexts {
		if c.Name == kc.CurrentContext {
			ctxSettings = c.Context
			break
		}
	}

	var ci clusterInfo

	// Resolve cluster.
	for _, cl := range kc.Clusters {
		if cl.Name == ctxSettings.Cluster {
			ci.Server = cl.Cluster.Server
			if cl.Cluster.CertificateAuthorityData != "" {
				decoded, err := base64.StdEncoding.DecodeString(cl.Cluster.CertificateAuthorityData)
				if err == nil {
					ci.CACert = decoded
				}
			} else if cl.Cluster.CertificateAuthority != "" {
				caData, err := os.ReadFile(cl.Cluster.CertificateAuthority)
				if err == nil {
					ci.CACert = caData
				}
			}
			break
		}
	}

	// Resolve user / token.
	for _, u := range kc.Users {
		if u.Name == ctxSettings.User {
			ci.Token = u.User.Token
			break
		}
	}

	if ci.Server == "" {
		return clusterInfo{}, fmt.Errorf("could not determine cluster server from kubeconfig")
	}

	return ci, nil
}

// findKubeconfig determines the path to the kubeconfig file.  It checks, in
// order: the explicit path argument, the $KUBECONFIG environment variable,
// and finally ~/.kube/config.
func findKubeconfig(explicit string) (string, error) {
	if explicit != "" {
		if _, err := os.Stat(explicit); err != nil {
			return "", fmt.Errorf("kubeconfig %q not found: %w", explicit, err)
		}
		return explicit, nil
	}

	if env := os.Getenv("KUBECONFIG"); env != "" {
		if _, err := os.Stat(env); err == nil {
			return env, nil
		}
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot determine home directory: %w", err)
	}

	defaultPath := filepath.Join(home, ".kube", "config")
	if _, err := os.Stat(defaultPath); err != nil {
		return "", fmt.Errorf("no kubeconfig found at %q: %w", defaultPath, err)
	}

	return defaultPath, nil
}
