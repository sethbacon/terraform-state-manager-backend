package statesource

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
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

func (h *hcp) Read(ctx context.Context, key string) (*RawState, error) {
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

// Write is not yet supported for HCP/TFE: editing managed-workspace state safely
// requires locking the workspace and creating a new state version via the run API.
func (h *hcp) Write(_ context.Context, _ string, _ []byte) error {
	return fmt.Errorf("editing state is not supported for HCP/TFE sources yet")
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
