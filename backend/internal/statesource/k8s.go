package statesource

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"
)

// k8s reads Terraform state stored by the kubernetes backend as Secrets (data
// key "tfstate"). Connection details come either from explicit config
// (server [+ ca_cert] + credentials.token) or from a kubeconfig file path —
// explicit settings win, so a server deployment never has to mount a kubeconfig.
type k8s struct {
	client    *http.Client
	server    string
	token     string
	namespace string
	labels    string
}

func newK8s(config, credentials map[string]any) (*k8s, error) {
	namespace, _ := config["namespace"].(string)
	if namespace == "" {
		namespace = "default"
	}
	labels, _ := config["labels"].(string)

	server, _ := config["server"].(string)
	token, _ := credentials["token"].(string)
	caPEM := []byte{}
	if ca, _ := config["ca_cert"].(string); ca != "" {
		caPEM = []byte(ca)
	}

	// Fallback: resolve from a kubeconfig file when no explicit server is given.
	if server == "" {
		path, _ := config["kubeconfig"].(string)
		ci, err := resolveKubeconfig(path)
		if err != nil {
			return nil, fmt.Errorf("kubernetes source requires config.server + credentials.token, or a readable kubeconfig: %w", err)
		}
		server = ci.server
		if token == "" {
			token = ci.token
		}
		if len(caPEM) == 0 {
			caPEM = ci.caCert
		}
	}
	u, err := url.Parse(server)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, fmt.Errorf("invalid kubernetes server URL %q", server)
	}

	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if len(caPEM) > 0 {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caPEM) {
			return nil, fmt.Errorf("kubernetes ca_cert is not valid PEM")
		}
		tlsCfg.RootCAs = pool
	}
	return &k8s{
		client: &http.Client{
			Timeout: connectorHTTPTimeout,
			// SSRF egress guard: dial only validated IPs (resolve-and-pin) and
			// re-validate redirects, while keeping the cluster CA's TLS config
			// (#256). The k8s API server is typically at a private IP, which the
			// guard allow-lists, so in-cluster reads keep working.
			Transport: &http.Transport{
				TLSClientConfig:   tlsCfg,
				DialContext:       egressGuard.DialContext,
				ForceAttemptHTTP2: true,
			},
			CheckRedirect: egressGuard.CheckRedirect,
		},
		server:    strings.TrimRight(server, "/"),
		token:     token,
		namespace: namespace,
		labels:    labels,
	}, nil
}

func (k *k8s) do(ctx context.Context, method, rawURL, contentType string, body io.Reader) (*http.Response, error) {
	return httpDo(ctx, k.client, method, rawURL, body, func(req *http.Request) {
		if k.token != "" {
			req.Header.Set("Authorization", "Bearer "+k.token)
		}
		req.Header.Set("Accept", "application/json")
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
	})
}

type k8sSecret struct {
	Metadata struct {
		Name              string `json:"name"`
		Namespace         string `json:"namespace"`
		CreationTimestamp string `json:"creationTimestamp"`
	} `json:"metadata"`
	Data map[string]string `json:"data"` // base64-encoded values
}

// List enumerates Secrets in the namespace (optionally label-filtered) that carry
// a "tfstate" data key. Keys take the form "namespace/name".
func (k *k8s) List(ctx context.Context) ([]StateRef, error) {
	params := url.Values{}
	if k.labels != "" {
		params.Set("labelSelector", k.labels)
	}
	listURL := fmt.Sprintf("%s/api/v1/namespaces/%s/secrets?%s", k.server, url.PathEscape(k.namespace), params.Encode())
	resp, err := k.do(ctx, http.MethodGet, listURL, "", nil)
	if err != nil {
		return nil, fmt.Errorf("kubernetes list failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := readCapped(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("kubernetes list read failed: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("kubernetes list returned status %d", resp.StatusCode)
	}
	var list struct {
		Items []k8sSecret `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("kubernetes list parse failed: %w", err)
	}
	refs := make([]StateRef, 0, len(list.Items))
	for _, s := range list.Items {
		encoded, ok := s.Data["tfstate"]
		if !ok {
			continue
		}
		decoded, dErr := base64.StdEncoding.DecodeString(encoded)
		if dErr != nil {
			continue
		}
		ref := StateRef{
			Key:  s.Metadata.Namespace + "/" + s.Metadata.Name,
			Name: s.Metadata.Name,
			Size: int64(len(decoded)),
		}
		if ts, tErr := time.Parse(time.RFC3339, s.Metadata.CreationTimestamp); tErr == nil {
			ref.LastModified = &ts
		}
		refs = append(refs, ref)
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].Key < refs[j].Key })
	return refs, nil
}

// secretURL maps a "namespace/name" key to the Secret resource URL.
func (k *k8s) secretURL(key string) (string, error) {
	parts := strings.SplitN(key, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", fmt.Errorf("invalid state key %q (expected namespace/name)", key)
	}
	return fmt.Sprintf("%s/api/v1/namespaces/%s/secrets/%s",
		k.server, url.PathEscape(parts[0]), url.PathEscape(parts[1])), nil
}

// k8sLog tags mutation logs from this connector.
var k8sLog = slog.With("component", "statesource.k8s")

func (k *k8s) Read(ctx context.Context, key string) (*RawState, error) {
	su, err := k.secretURL(key)
	if err != nil {
		return nil, err
	}
	resp, err := k.do(ctx, http.MethodGet, su, "", nil)
	if err != nil {
		return nil, fmt.Errorf("kubernetes read failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := readCapped(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("kubernetes read body failed: %w", err)
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("state %q %w", key, ErrNotFound)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("kubernetes read returned status %d", resp.StatusCode)
	}
	var s k8sSecret
	if err := json.Unmarshal(body, &s); err != nil {
		return nil, fmt.Errorf("kubernetes secret parse failed: %w", err)
	}
	encoded, ok := s.Data["tfstate"]
	if !ok {
		return nil, fmt.Errorf("secret %q has no tfstate key", key)
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("tfstate decode failed: %w", err)
	}
	return &RawState{Key: key, Data: decoded, Size: int64(len(decoded))}, nil
}

// Write replaces the secret's tfstate key via a strategic-merge PATCH, leaving
// other data keys and metadata untouched.
func (k *k8s) Write(ctx context.Context, key string, data []byte) error {
	su, err := k.secretURL(key)
	if err != nil {
		return err
	}
	patch, err := json.Marshal(map[string]any{
		"data": map[string]string{"tfstate": base64.StdEncoding.EncodeToString(data)},
	})
	if err != nil {
		return err
	}
	resp, err := k.do(ctx, http.MethodPatch, su, "application/strategic-merge-patch+json", bytes.NewReader(patch))
	if err != nil {
		return fmt.Errorf("kubernetes write failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		// New state key (e.g. a transfer target): create the Secret instead.
		return k.createSecret(ctx, key, data)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("kubernetes write returned status %d: %s", resp.StatusCode, string(body))
	}
	return nil
}

// createSecret creates a new Opaque Secret holding the state, labelled so its
// origin is traceable. Only called when a write targets a missing Secret.
func (k *k8s) createSecret(ctx context.Context, key string, data []byte) error {
	parts := strings.SplitN(key, "/", 2)
	if len(parts) != 2 {
		return fmt.Errorf("invalid state key %q (expected namespace/name)", key)
	}
	manifest, err := json.Marshal(map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata": map[string]any{
			"name":      parts[1],
			"namespace": parts[0],
			"labels":    map[string]string{"app.kubernetes.io/managed-by": "terraform-state-manager"},
		},
		"type": "Opaque",
		"data": map[string]string{"tfstate": base64.StdEncoding.EncodeToString(data)},
	})
	if err != nil {
		return err
	}
	createURL := fmt.Sprintf("%s/api/v1/namespaces/%s/secrets", k.server, url.PathEscape(parts[0]))
	resp, err := k.do(ctx, http.MethodPost, createURL, "application/json", bytes.NewReader(manifest))
	if err != nil {
		return fmt.Errorf("kubernetes secret create failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("kubernetes secret create returned status %d: %s", resp.StatusCode, string(body))
	}
	k8sLog.Info("created state secret", "namespace", parts[0], "name", parts[1], "bytes", len(data))
	return nil
}

// Delete removes the Secret holding the state at key. A missing Secret is
// reported as ErrNotFound.
func (k *k8s) Delete(ctx context.Context, key string) error {
	su, err := k.secretURL(key)
	if err != nil {
		return err
	}
	resp, err := k.do(ctx, http.MethodDelete, su, "", nil)
	if err != nil {
		return fmt.Errorf("kubernetes delete failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("state %q %w", key, ErrNotFound)
	}
	// Kubernetes returns 200 (Status) or 202 (Accepted, finalizers pending).
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("kubernetes delete returned status %d: %s", resp.StatusCode, string(body))
	}
	k8sLog.Info("deleted state secret", "key", key)
	return nil
}

// --- kubeconfig fallback ---

type kubeClusterInfo struct {
	server string
	token  string
	caCert []byte
}

// resolveKubeconfig extracts the current context's server, bearer token, and CA
// from a kubeconfig file (explicit path, then $KUBECONFIG, then ~/.kube/config).
// Only JSON-formatted kubeconfigs are supported, matching the original scanner.
func resolveKubeconfig(explicit string) (kubeClusterInfo, error) {
	path, err := findKubeconfigPath(explicit)
	if err != nil {
		return kubeClusterInfo{}, err
	}
	data, err := os.ReadFile(path) // #nosec G304 -- path is operator-configured or a standard kubeconfig location
	if err != nil {
		return kubeClusterInfo{}, fmt.Errorf("failed to read kubeconfig %q: %w", path, err)
	}

	var kc struct {
		Clusters []struct {
			Name    string `json:"name"`
			Cluster struct {
				Server                   string `json:"server"`
				CertificateAuthority     string `json:"certificate-authority"`
				CertificateAuthorityData string `json:"certificate-authority-data"`
			} `json:"cluster"`
		} `json:"clusters"`
		Users []struct {
			Name string `json:"name"`
			User struct {
				Token string `json:"token"`
			} `json:"user"`
		} `json:"users"`
		Contexts []struct {
			Name    string `json:"name"`
			Context struct {
				Cluster string `json:"cluster"`
				User    string `json:"user"`
			} `json:"context"`
		} `json:"contexts"`
		CurrentContext string `json:"current-context"`
	}
	if err := json.Unmarshal(data, &kc); err != nil {
		return kubeClusterInfo{}, fmt.Errorf("failed to parse kubeconfig (JSON): %w", err)
	}

	current := kc.CurrentContext
	if current == "" && len(kc.Contexts) > 0 {
		current = kc.Contexts[0].Name
	}
	var clusterName, userName string
	for _, c := range kc.Contexts {
		if c.Name == current {
			clusterName, userName = c.Context.Cluster, c.Context.User
			break
		}
	}

	var ci kubeClusterInfo
	for _, cl := range kc.Clusters {
		if cl.Name != clusterName {
			continue
		}
		ci.server = cl.Cluster.Server
		if cl.Cluster.CertificateAuthorityData != "" {
			if decoded, dErr := base64.StdEncoding.DecodeString(cl.Cluster.CertificateAuthorityData); dErr == nil {
				ci.caCert = decoded
			}
		} else if cl.Cluster.CertificateAuthority != "" {
			// The CA path comes from the operator-trusted kubeconfig file itself
			// (same trust boundary as its token/server); the bytes are only used
			// as a TLS root CA, never echoed.
			if caData, rErr := os.ReadFile(cl.Cluster.CertificateAuthority); rErr == nil { // #nosec G304 G703 -- operator-trusted kubeconfig
				ci.caCert = caData
			}
		}
		break
	}
	for _, u := range kc.Users {
		if u.Name == userName {
			ci.token = u.User.Token
			break
		}
	}
	if ci.server == "" {
		return kubeClusterInfo{}, fmt.Errorf("could not determine cluster server from kubeconfig")
	}
	return ci, nil
}

// findKubeconfigPath only accepts an explicitly configured path. Unlike kubectl
// (and the original scanner), the server deliberately does NOT probe $KUBECONFIG
// or ~/.kube/config: a service should never pick up ambient credentials that the
// operator didn't knowingly hand it.
func findKubeconfigPath(explicit string) (string, error) {
	if explicit == "" {
		return "", fmt.Errorf("no config.kubeconfig path set")
	}
	if _, err := os.Stat(explicit); err != nil {
		return "", fmt.Errorf("kubeconfig %q not found: %w", explicit, err)
	}
	return explicit, nil
}
