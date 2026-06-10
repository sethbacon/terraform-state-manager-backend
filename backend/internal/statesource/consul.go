package statesource

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// consul reads Terraform state stored by the consul backend in the Consul KV
// store under a configurable path prefix. The ACL token is a credential.
type consul struct {
	client     *http.Client
	scheme     string
	address    string
	datacenter string
	path       string
	token      string
}

func newConsul(config, credentials map[string]any) (*consul, error) {
	addr, _ := config["address"].(string)
	if addr == "" {
		return nil, fmt.Errorf("consul source requires config.address (host:port)")
	}
	scheme, _ := config["scheme"].(string)
	if scheme == "" {
		scheme = "http"
	}
	if scheme != "http" && scheme != "https" {
		return nil, fmt.Errorf("consul scheme must be \"http\" or \"https\"")
	}
	path, _ := config["path"].(string)
	path = strings.Trim(path, "/")
	if path == "" {
		path = "terraform"
	}
	dc, _ := config["datacenter"].(string)
	token, _ := credentials["token"].(string)
	return &consul{
		client:     &http.Client{Timeout: 30 * time.Second},
		scheme:     scheme,
		address:    addr,
		datacenter: dc,
		path:       path,
		token:      token,
	}, nil
}

// kvURL builds a Consul KV API URL. The key is placed in url.URL.Path so each
// segment is escaped correctly while "/" separators are preserved (escaping the
// whole key with PathEscape would break nested KV paths).
func (c *consul) kvURL(key string, query url.Values) string {
	if c.datacenter != "" {
		query.Set("dc", c.datacenter)
	}
	u := url.URL{Scheme: c.scheme, Host: c.address, Path: "/v1/kv/" + key, RawQuery: query.Encode()}
	return u.String()
}

func (c *consul) do(ctx context.Context, method, rawURL string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, rawURL, body)
	if err != nil {
		return nil, err
	}
	if c.token != "" {
		req.Header.Set("X-Consul-Token", c.token)
	}
	return c.client.Do(req)
}

// consulKVEntry is one KV pair from the non-raw GET endpoint (Value is base64).
type consulKVEntry struct {
	Key         string `json:"Key"`
	Value       string `json:"Value"`
	ModifyIndex int64  `json:"ModifyIndex"`
}

// List enumerates KV entries under the configured path with a single recursive
// query (the per-key metadata round-trips the original scanner did are not needed).
func (c *consul) List(ctx context.Context) ([]StateRef, error) {
	resp, err := c.do(ctx, http.MethodGet, c.kvURL(c.path, url.Values{"recurse": {"true"}}), nil)
	if err != nil {
		return nil, fmt.Errorf("consul list failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil // path does not exist yet
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("consul list read failed: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("consul list returned status %d", resp.StatusCode)
	}
	var entries []consulKVEntry
	if err := json.Unmarshal(body, &entries); err != nil {
		return nil, fmt.Errorf("consul list parse failed: %w", err)
	}
	refs := make([]StateRef, 0, len(entries))
	for _, e := range entries {
		if strings.HasSuffix(e.Key, "/") {
			continue // directory placeholder
		}
		size := int64(0)
		if decoded, dErr := base64.StdEncoding.DecodeString(e.Value); dErr == nil {
			size = int64(len(decoded))
		}
		refs = append(refs, StateRef{Key: e.Key, Name: e.Key, Size: size})
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].Key < refs[j].Key })
	return refs, nil
}

func (c *consul) Read(ctx context.Context, key string) (*RawState, error) {
	resp, err := c.do(ctx, http.MethodGet, c.kvURL(key, url.Values{"raw": {"true"}}), nil)
	if err != nil {
		return nil, fmt.Errorf("consul read failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("state %q not found", key)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("consul read returned status %d", resp.StatusCode)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("consul read body failed: %w", err)
	}
	return &RawState{Key: key, Data: data, Size: int64(len(data))}, nil
}

func (c *consul) Write(ctx context.Context, key string, data []byte) error {
	resp, err := c.do(ctx, http.MethodPut, c.kvURL(key, url.Values{}), bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("consul write failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("consul write returned status %d", resp.StatusCode)
	}
	if strings.TrimSpace(string(body)) != "true" {
		return fmt.Errorf("consul rejected the write")
	}
	return nil
}
