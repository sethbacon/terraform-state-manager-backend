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
	"strconv"
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
	body, err := readCapped(resp.Body)
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
		if strings.HasSuffix(e.Key, consulLockSuffix) || strings.HasSuffix(e.Key, "/.lockinfo") {
			continue // lock artifacts (TSM sessions, terraform's consul backend), not state
		}
		size := int64(0)
		if decoded, dErr := base64.StdEncoding.DecodeString(e.Value); dErr == nil {
			size = int64(len(decoded))
		}
		refs = append(refs, StateRef{Key: e.Key, Name: e.Key, Size: size, Version: strconv.FormatInt(e.ModifyIndex, 10)})
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
		return nil, fmt.Errorf("state %q %w", key, ErrNotFound)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("consul read returned status %d", resp.StatusCode)
	}
	data, err := readCapped(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("consul read body failed: %w", err)
	}
	return &RawState{Key: key, Data: data, Size: int64(len(data))}, nil
}

// Write replaces the state via Consul's check-and-set: the key's current
// ModifyIndex is fetched and presented as ?cas=, so a write racing another
// writer (a terraform apply against the consul backend, another edit) is
// rejected by Consul instead of silently overwriting it. cas=0 means
// create-only, covering fresh keys.
func (c *consul) Write(ctx context.Context, key string, data []byte) error {
	idx, err := c.modifyIndex(ctx, key)
	if err != nil {
		return err
	}
	resp, err := c.do(ctx, http.MethodPut, c.kvURL(key, url.Values{"cas": {strconv.FormatInt(idx, 10)}}), bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("consul write failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("consul write returned status %d", resp.StatusCode)
	}
	if strings.TrimSpace(string(body)) != "true" {
		return fmt.Errorf("consul rejected the write: key %q changed since it was read (concurrent writer) — re-read and retry", key)
	}
	return nil
}

// Delete removes the KV entry at key. Consul's delete is idempotent (a missing
// key still returns 200); the edit pipeline's pre-delete read enforces the
// not-found case before this is called.
func (c *consul) Delete(ctx context.Context, key string) error {
	resp, err := c.do(ctx, http.MethodDelete, c.kvURL(key, url.Values{}), nil)
	if err != nil {
		return fmt.Errorf("consul delete failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("consul delete returned status %d", resp.StatusCode)
	}
	return nil
}

// --- Locker (Consul session lock) ---

// consulLockSuffix is appended to the state key to form the lock key. It is
// the same key Terraform's consul backend locks (<path>/.lock), so a TSM edit
// and a concurrent `terraform apply` mutually exclude each other. This closes
// the read-to-write CAS gap: Write's ?cas index is fetched at write time, so
// on its own it cannot see a writer that committed between the edit pipeline's
// read and the write — under this lock no such writer can commit.
const consulLockSuffix = "/.lock"

// consulSessionTTL bounds an orphaned lock: the session is created with
// Behavior=release, so if TSM crashes mid-edit Consul expires the session and
// releases the lock server-side. Matches the app-level advisory lock's
// 15-minute stale TTL (ADR 003).
const consulSessionTTL = "15m"

// sessionURL builds a Consul session API URL (datacenter-aware like kvURL).
func (c *consul) sessionURL(p string) string {
	q := url.Values{}
	if c.datacenter != "" {
		q.Set("dc", c.datacenter)
	}
	u := url.URL{Scheme: c.scheme, Host: c.address, Path: p, RawQuery: q.Encode()}
	return u.String()
}

// Lock implements Locker: a fresh Consul session acquires <key>/.lock. The
// session id doubles as the lock id. A rejected acquire means another editor
// or a terraform operation holds the lock — the edit pipeline surfaces that
// as a 409 conflict.
func (c *consul) Lock(ctx context.Context, key string) (string, error) {
	body, _ := json.Marshal(map[string]string{
		"Name":     "terraform-state-manager state edit",
		"TTL":      consulSessionTTL,
		"Behavior": "release",
	})
	resp, err := c.do(ctx, http.MethodPut, c.sessionURL("/v1/session/create"), bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("consul session create failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("consul session create returned status %d", resp.StatusCode)
	}
	var sess struct {
		ID string `json:"ID"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&sess); err != nil || sess.ID == "" {
		return "", fmt.Errorf("consul session create returned no session id (%v)", err)
	}

	info, _ := json.Marshal(map[string]string{"Holder": "terraform-state-manager", "Operation": "state edit"})
	aResp, err := c.do(ctx, http.MethodPut, c.kvURL(key+consulLockSuffix, url.Values{"acquire": {sess.ID}}), bytes.NewReader(info))
	if err != nil {
		c.destroySession(ctx, sess.ID)
		return "", fmt.Errorf("consul lock acquire failed: %w", err)
	}
	defer func() { _ = aResp.Body.Close() }()
	ok, _ := io.ReadAll(io.LimitReader(aResp.Body, 64))
	if aResp.StatusCode != http.StatusOK || strings.TrimSpace(string(ok)) != "true" {
		c.destroySession(ctx, sess.ID)
		return "", fmt.Errorf("state %q is locked in Consul (another edit or a terraform operation holds %s)", key, key+consulLockSuffix)
	}
	return sess.ID, nil
}

// Unlock releases <key>/.lock and destroys the lock's session. Both steps are
// tolerant of an already-expired session (Consul released the lock itself).
func (c *consul) Unlock(ctx context.Context, key, lockID string) error {
	resp, err := c.do(ctx, http.MethodPut, c.kvURL(key+consulLockSuffix, url.Values{"release": {lockID}}), nil)
	if err == nil {
		_ = resp.Body.Close()
	}
	c.destroySession(ctx, lockID)
	if err != nil {
		return fmt.Errorf("consul lock release failed: %w", err)
	}
	return nil
}

// destroySession is best-effort teardown; an expired session is already gone.
func (c *consul) destroySession(ctx context.Context, id string) {
	if resp, err := c.do(ctx, http.MethodPut, c.sessionURL("/v1/session/destroy/"+id), nil); err == nil {
		_ = resp.Body.Close()
	}
}

// modifyIndex returns the key's current ModifyIndex for check-and-set writes,
// or 0 (create-only) when the key does not exist.
func (c *consul) modifyIndex(ctx context.Context, key string) (int64, error) {
	resp, err := c.do(ctx, http.MethodGet, c.kvURL(key, url.Values{}), nil)
	if err != nil {
		return 0, fmt.Errorf("consul cas-index read failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return 0, nil
	}
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("consul cas-index read returned status %d", resp.StatusCode)
	}
	var entries []consulKVEntry
	if err := json.NewDecoder(resp.Body).Decode(&entries); err != nil {
		return 0, fmt.Errorf("consul cas-index parse failed: %w", err)
	}
	if len(entries) == 0 {
		return 0, nil
	}
	return entries[0].ModifyIndex, nil
}
