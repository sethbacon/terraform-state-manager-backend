package statesource

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/md5" // #nosec G501 -- HCP's state-version API requires an MD5 checksum
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
)

// hcp reads state from HCP Terraform / Terraform Enterprise via the v2 API.
// Workspaces are listed for an organization; each workspace's current state
// version is downloaded on demand. The key is the workspace ID.
type hcp struct {
	client  *http.Client
	baseURL string
	org     string
	token   string
}

func newHCP(config, credentials map[string]any) (*hcp, error) {
	org, _ := config["organization"].(string)
	if org == "" {
		return nil, fmt.Errorf("hcp source requires config.organization")
	}
	host, _ := config["hostname"].(string)
	if host == "" {
		host = "app.terraform.io"
	}
	token, _ := credentials["token"].(string)
	if token == "" {
		return nil, fmt.Errorf("hcp source requires a credentials.token (HCP Terraform API token)")
	}
	return &hcp{
		client:  &http.Client{Timeout: 30 * time.Second},
		baseURL: "https://" + host,
		org:     org,
		token:   token,
	}, nil
}

func (h *hcp) List(ctx context.Context) ([]StateRef, error) {
	next := fmt.Sprintf("%s/api/v2/organizations/%s/workspaces?page[size]=100", h.baseURL, url.PathEscape(h.org))
	var refs []StateRef
	for next != "" {
		var resp struct {
			Data []struct {
				ID         string `json:"id"`
				Attributes struct {
					Name      string `json:"name"`
					UpdatedAt string `json:"updated-at"`
				} `json:"attributes"`
				Relationships struct {
					CurrentStateVersion json.RawMessage `json:"current-state-version"`
				} `json:"relationships"`
			} `json:"data"`
			Links struct {
				Next string `json:"next"`
			} `json:"links"`
		}
		if err := h.getJSON(ctx, next, &resp); err != nil {
			return nil, err
		}
		for _, ws := range resp.Data {
			// Skip workspaces that have never stored state (the relationship is
			// present with a null data pointer): nothing to read or analyze.
			// Absent relationship (older TFE payloads) -> include, Read decides.
			if len(ws.Relationships.CurrentStateVersion) > 0 {
				var rel struct {
					Data *struct {
						ID string `json:"id"`
					} `json:"data"`
				}
				if err := json.Unmarshal(ws.Relationships.CurrentStateVersion, &rel); err == nil && rel.Data == nil {
					continue
				}
			}
			ref := StateRef{Key: ws.ID, Name: ws.Attributes.Name}
			if t, err := time.Parse(time.RFC3339, ws.Attributes.UpdatedAt); err == nil {
				ref.LastModified = &t
			}
			refs = append(refs, ref)
		}
		next = resp.Links.Next
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].Name < refs[j].Name })
	return refs, nil
}

// resolveWorkspaceID returns the workspace ID for a key: ws-… keys pass
// through; anything else is treated as a workspace NAME and looked up.
func (h *hcp) resolveWorkspaceID(ctx context.Context, key string) (string, error) {
	if strings.HasPrefix(key, "ws-") {
		return key, nil
	}
	ws, err := h.workspaceByName(ctx, key)
	if err != nil {
		return "", err
	}
	if ws == nil {
		return "", fmt.Errorf("workspace %q not found in organization %s", key, h.org)
	}
	return ws.ID, nil
}

type hcpWorkspace struct {
	ID     string `json:"id"`
	Locked bool
}

// workspaceByName looks a workspace up by its friendly name; (nil, nil) when absent.
func (h *hcp) workspaceByName(ctx context.Context, name string) (*hcpWorkspace, error) {
	var resp struct {
		Data struct {
			ID         string `json:"id"`
			Attributes struct {
				Locked bool `json:"locked"`
			} `json:"attributes"`
		} `json:"data"`
	}
	u := fmt.Sprintf("%s/api/v2/organizations/%s/workspaces/%s", h.baseURL, url.PathEscape(h.org), url.PathEscape(name))
	if err := h.getJSON(ctx, u, &resp); err != nil {
		if strings.Contains(err.Error(), "returned 404") {
			return nil, nil
		}
		return nil, err
	}
	return &hcpWorkspace{ID: resp.Data.ID, Locked: resp.Data.Attributes.Locked}, nil
}

func (h *hcp) Read(ctx context.Context, key string) (*RawState, error) {
	wsID, err := h.resolveWorkspaceID(ctx, key)
	if err != nil {
		return nil, err
	}
	key = wsID
	var sv struct {
		Data struct {
			Attributes struct {
				DownloadURL string `json:"hosted-state-download-url"`
				Serial      int64  `json:"serial"`
			} `json:"attributes"`
		} `json:"data"`
	}
	u := fmt.Sprintf("%s/api/v2/workspaces/%s/current-state-version", h.baseURL, url.PathEscape(key))
	if err := h.getJSON(ctx, u, &sv); err != nil {
		return nil, fmt.Errorf("failed to read current state version for workspace %s: %w", key, err)
	}
	if sv.Data.Attributes.DownloadURL == "" {
		return nil, fmt.Errorf("workspace %s has no current state version", key)
	}
	data, err := h.download(ctx, sv.Data.Attributes.DownloadURL)
	if err != nil {
		return nil, err
	}
	return &RawState{Key: key, Data: data, Size: int64(len(data))}, nil
}

// hcpWorkspaceName validates names for create-on-write (HCP allows letters,
// numbers, hyphens, underscores).
var hcpWorkspaceName = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// hcpLog tags mutation logs from this connector.
var hcpLog = slog.With("component", "statesource.hcp")

// Write creates a new state version via the HCP API: resolve (or create) the
// workspace, verify the serial advances and the lineage matches, then
// lock -> upload state version -> unlock. A ws-… key must already exist; any
// other key is treated as a workspace NAME and is created when absent (this is
// what makes "transfer to HCP" mint a workspace).
func (h *hcp) Write(ctx context.Context, key string, data []byte) error {
	// The state's own serial/lineage drive the version attributes.
	var meta struct {
		Serial  int64  `json:"serial"`
		Lineage string `json:"lineage"`
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		return fmt.Errorf("state is not valid JSON: %w", err)
	}

	wsID, err := h.ensureWorkspace(ctx, key)
	if err != nil {
		return err
	}

	// Guard rails before touching anything: the new serial must advance past
	// the workspace's current state, and the lineage must match. HCP enforces
	// both server-side; checking here yields actionable errors.
	if cur, err := h.currentStateMeta(ctx, wsID); err != nil {
		return err
	} else if cur != nil {
		if meta.Serial <= cur.Serial {
			return fmt.Errorf("workspace %s already has state at serial %d; the state being written has serial %d — increase the serial to overwrite", key, cur.Serial, meta.Serial)
		}
		if cur.Lineage != "" && meta.Lineage != "" && cur.Lineage != meta.Lineage {
			return fmt.Errorf("lineage mismatch: workspace %s tracks lineage %s but the state being written has %s — refusing to overwrite a different state's history", key, cur.Lineage, meta.Lineage)
		}
	}

	if err := h.lockWorkspace(ctx, wsID); err != nil {
		return err
	}
	defer func() {
		if err := h.unlockWorkspace(context.WithoutCancel(ctx), wsID); err != nil {
			hcpLog.Error("failed to unlock workspace after write", "workspace", wsID, "error", err)
		}
	}()

	sum := md5.Sum(data) // #nosec G401 -- HCP's state-version API requires an MD5 content checksum
	body, _ := json.Marshal(map[string]any{
		"data": map[string]any{
			"type": "state-versions",
			"attributes": map[string]any{
				"serial":  meta.Serial,
				"md5":     hex.EncodeToString(sum[:]),
				"lineage": meta.Lineage,
				"state":   base64.StdEncoding.EncodeToString(data),
			},
		},
	})
	u := fmt.Sprintf("%s/api/v2/workspaces/%s/state-versions", h.baseURL, url.PathEscape(wsID))
	if err := h.postJSON(ctx, u, body, nil); err != nil {
		return fmt.Errorf("failed to create state version in workspace %s: %w", key, err)
	}
	hcpLog.Info("state version created", "workspace", wsID, "key", key, "serial", meta.Serial, "bytes", len(data))
	return nil
}

// ensureWorkspace resolves the write target: ws-… IDs must exist; names are
// looked up and created when absent.
func (h *hcp) ensureWorkspace(ctx context.Context, key string) (string, error) {
	if strings.HasPrefix(key, "ws-") {
		return key, nil
	}
	ws, err := h.workspaceByName(ctx, key)
	if err != nil {
		return "", err
	}
	if ws != nil {
		return ws.ID, nil
	}
	if !hcpWorkspaceName.MatchString(key) {
		return "", fmt.Errorf("invalid workspace name %q (letters, numbers, hyphens, and underscores only)", key)
	}
	body, _ := json.Marshal(map[string]any{
		"data": map[string]any{
			"type":       "workspaces",
			"attributes": map[string]any{"name": key},
		},
	})
	var created struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	u := fmt.Sprintf("%s/api/v2/organizations/%s/workspaces", h.baseURL, url.PathEscape(h.org))
	if err := h.postJSON(ctx, u, body, &created); err != nil {
		return "", fmt.Errorf("failed to create workspace %q: %w", key, err)
	}
	hcpLog.Info("workspace created", "organization", h.org, "name", key, "id", created.Data.ID)
	return created.Data.ID, nil
}

type hcpStateMeta struct {
	Serial  int64
	Lineage string
}

// currentStateMeta returns the workspace's current serial/lineage, or nil when
// it has no state yet.
func (h *hcp) currentStateMeta(ctx context.Context, wsID string) (*hcpStateMeta, error) {
	var sv struct {
		Data struct {
			Attributes struct {
				Serial  int64  `json:"serial"`
				Lineage string `json:"lineage"`
			} `json:"attributes"`
		} `json:"data"`
	}
	u := fmt.Sprintf("%s/api/v2/workspaces/%s/current-state-version", h.baseURL, url.PathEscape(wsID))
	if err := h.getJSON(ctx, u, &sv); err != nil {
		if strings.Contains(err.Error(), "returned 404") {
			return nil, nil // fresh workspace
		}
		return nil, err
	}
	return &hcpStateMeta{Serial: sv.Data.Attributes.Serial, Lineage: sv.Data.Attributes.Lineage}, nil
}

func (h *hcp) lockWorkspace(ctx context.Context, wsID string) error {
	body, _ := json.Marshal(map[string]any{"reason": "terraform-state-manager state write"})
	u := fmt.Sprintf("%s/api/v2/workspaces/%s/actions/lock", h.baseURL, url.PathEscape(wsID))
	if err := h.postJSON(ctx, u, body, nil); err != nil {
		return fmt.Errorf("failed to lock workspace %s (is it locked by a run or another user?): %w", wsID, err)
	}
	hcpLog.Info("workspace locked for write", "workspace", wsID)
	return nil
}

func (h *hcp) unlockWorkspace(ctx context.Context, wsID string) error {
	u := fmt.Sprintf("%s/api/v2/workspaces/%s/actions/unlock", h.baseURL, url.PathEscape(wsID))
	if err := h.postJSON(ctx, u, []byte(`{}`), nil); err != nil {
		return err
	}
	hcpLog.Info("workspace unlocked", "workspace", wsID)
	return nil
}

// postJSON issues an authenticated JSON:API POST; out may be nil.
func (h *hcp) postJSON(ctx context.Context, u string, body []byte, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+h.token)
	req.Header.Set("Content-Type", "application/vnd.api+json")
	resp, err := h.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("HCP API %s returned %d: %s", u, resp.StatusCode, string(b))
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func (h *hcp) getJSON(ctx context.Context, u string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+h.token)
	req.Header.Set("Content-Type", "application/vnd.api+json")
	resp, err := h.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("HCP API %s returned %d: %s", u, resp.StatusCode, string(body))
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// download fetches the (authenticated) state download URL and transparently
// decompresses a gzip body.
func (h *hcp) download(ctx context.Context, u string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+h.token)
	resp, err := h.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("state download returned %d", resp.StatusCode)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if len(data) >= 2 && data[0] == 0x1f && data[1] == 0x8b {
		gz, gzErr := gzip.NewReader(bytes.NewReader(data))
		if gzErr != nil {
			return nil, gzErr
		}
		defer func() { _ = gz.Close() }()
		return io.ReadAll(gz)
	}
	return data, nil
}
