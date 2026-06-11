package statesource

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// httpBackend reads the single Terraform state served by the standard http
// backend: GET fetches it, the configured update method (default POST) replaces
// it, and the optional LOCK/UNLOCK endpoints provide advisory locking. Basic-auth
// credentials are stored encrypted like other connector secrets.
type httpBackend struct {
	client        *http.Client
	address       string
	lockAddress   string
	unlockAddress string
	updateMethod  string
	username      string
	password      string
}

// httpStateKey is the stable key for the backend's single state.
const httpStateKey = "default"

// httpBackendWithLock wraps httpBackend with the optional Locker implementation.
// The base type deliberately does NOT implement Locker: the edit pipeline treats
// any Locker error as "already locked" (409), so a backend without a
// lock_address must fall back to the app-level DB lock instead.
type httpBackendWithLock struct{ *httpBackend }

// newHTTPBackend returns a plain connector, or a locking one when a
// lock_address is configured.
func newHTTPBackend(config, credentials map[string]any) (Connector, error) {
	h, err := newHTTPBackendBase(config, credentials)
	if err != nil {
		return nil, err
	}
	if h.lockAddress != "" {
		return &httpBackendWithLock{httpBackend: h}, nil
	}
	return h, nil
}

func newHTTPBackendBase(config, credentials map[string]any) (*httpBackend, error) {
	address, _ := config["address"].(string)
	if address == "" {
		return nil, fmt.Errorf("http source requires config.address (state URL)")
	}
	if err := validateHTTPURL(address); err != nil {
		return nil, fmt.Errorf("invalid address: %w", err)
	}
	lockAddress, _ := config["lock_address"].(string)
	if lockAddress != "" {
		if err := validateHTTPURL(lockAddress); err != nil {
			return nil, fmt.Errorf("invalid lock_address: %w", err)
		}
	}
	unlockAddress, _ := config["unlock_address"].(string)
	if unlockAddress != "" {
		if err := validateHTTPURL(unlockAddress); err != nil {
			return nil, fmt.Errorf("invalid unlock_address: %w", err)
		}
	}
	updateMethod, _ := config["update_method"].(string)
	if updateMethod == "" {
		updateMethod = http.MethodPost
	}
	updateMethod = strings.ToUpper(updateMethod)
	if updateMethod != http.MethodPost && updateMethod != http.MethodPut && updateMethod != http.MethodPatch {
		return nil, fmt.Errorf("update_method must be POST, PUT, or PATCH")
	}
	username, _ := credentials["username"].(string)
	password, _ := credentials["password"].(string)
	return &httpBackend{
		client:        &http.Client{Timeout: 30 * time.Second},
		address:       address,
		lockAddress:   lockAddress,
		unlockAddress: unlockAddress,
		updateMethod:  updateMethod,
		username:      username,
		password:      password,
	}, nil
}

func validateHTTPURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("%q is not a valid http(s) URL", raw)
	}
	return nil
}

func (h *httpBackend) do(ctx context.Context, method, rawURL string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, rawURL, body)
	if err != nil {
		return nil, err
	}
	if h.username != "" || h.password != "" {
		req.SetBasicAuth(h.username, h.password)
	}
	return h.client.Do(req)
}

// List probes the address with HEAD: the http backend serves exactly one state,
// so the result is zero refs (no state yet) or one ref keyed "default".
func (h *httpBackend) List(ctx context.Context) ([]StateRef, error) {
	resp, err := h.do(ctx, http.MethodHead, h.address, nil)
	if err != nil {
		return nil, fmt.Errorf("http backend probe failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusNoContent {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("http backend probe returned status %d", resp.StatusCode)
	}
	ref := StateRef{Key: httpStateKey, Name: h.address}
	// HEAD without Content-Length reports -1; leave 0 so the API layer can
	// overlay the size recorded by the analysis store.
	if resp.ContentLength > 0 {
		ref.Size = resp.ContentLength
	}
	if lm := resp.Header.Get("Last-Modified"); lm != "" {
		if parsed, pErr := http.ParseTime(lm); pErr == nil {
			ref.LastModified = &parsed
		}
	}
	return []StateRef{ref}, nil
}

func (h *httpBackend) Read(ctx context.Context, key string) (*RawState, error) {
	resp, err := h.do(ctx, http.MethodGet, h.address, nil)
	if err != nil {
		return nil, fmt.Errorf("http backend read failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusNoContent {
		return nil, fmt.Errorf("state %q not found", key)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("http backend read returned status %d", resp.StatusCode)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("http backend read body failed: %w", err)
	}
	return &RawState{Key: key, Data: data, Size: int64(len(data))}, nil
}

// Write replaces the state via the backend's update method (default POST).
func (h *httpBackend) Write(ctx context.Context, _ string, data []byte) error {
	resp, err := h.do(ctx, h.updateMethod, h.address, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("http backend write failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("http backend write returned status %d: %s", resp.StatusCode, string(body))
	}
	return nil
}

// Lock acquires the backend's advisory lock with Terraform's LOCK verb, sending
// a lock-info body whose ID is returned for Unlock. Only the wrapper type
// implements Locker, so connectors without a lock_address fall back to the
// app-level DB lock.
func (h *httpBackendWithLock) Lock(ctx context.Context, _ string) (string, error) {
	idBytes := make([]byte, 8)
	if _, err := rand.Read(idBytes); err != nil {
		return "", err
	}
	lockID := hex.EncodeToString(idBytes)
	info, _ := json.Marshal(map[string]string{
		"ID": lockID, "Operation": "tsm-edit", "Who": "terraform-state-manager",
		"Created": time.Now().UTC().Format(time.RFC3339),
	})
	resp, err := h.do(ctx, "LOCK", h.lockAddress, bytes.NewReader(info))
	if err != nil {
		return "", fmt.Errorf("http backend lock failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusOK:
		return lockID, nil
	case http.StatusConflict, http.StatusLocked:
		return "", fmt.Errorf("state is locked by another holder")
	default:
		return "", fmt.Errorf("http backend lock returned status %d", resp.StatusCode)
	}
}

// Unlock releases the advisory lock with the UNLOCK verb (falling back to the
// lock address when no separate unlock_address is configured).
func (h *httpBackendWithLock) Unlock(ctx context.Context, _ string, lockID string) error {
	target := h.unlockAddress
	if target == "" {
		target = h.lockAddress
	}
	info, _ := json.Marshal(map[string]string{"ID": lockID})
	resp, err := h.do(ctx, "UNLOCK", target, bytes.NewReader(info))
	if err != nil {
		return fmt.Errorf("http backend unlock failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("http backend unlock returned status %d", resp.StatusCode)
	}
	return nil
}
