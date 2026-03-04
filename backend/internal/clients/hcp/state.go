package hcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// StateFile represents a parsed Terraform state file. It supports both the
// modern v3/v4 format (with top-level resources) and legacy v0-v2 format
// (with nested modules).
type StateFile struct {
	Version          int             `json:"version"`
	TerraformVersion string          `json:"terraform_version"`
	Serial           int             `json:"serial"`
	Lineage          string          `json:"lineage"`
	Resources        []StateResource `json:"resources"`
	Modules          []StateModule   `json:"modules,omitempty"`
}

// StateResource represents a single resource entry in a v3/v4 state file.
type StateResource struct {
	Mode      string          `json:"mode"`
	Type      string          `json:"type"`
	Name      string          `json:"name"`
	Provider  string          `json:"provider"`
	Module    string          `json:"module,omitempty"`
	Instances []StateInstance `json:"instances"`
}

// StateInstance holds the attribute data for a single resource instance.
type StateInstance struct {
	Attributes          json.RawMessage `json:"attributes,omitempty"`
	SensitiveAttributes json.RawMessage `json:"sensitive_attributes,omitempty"`
}

// StateModule represents a module entry in legacy v0-v2 state format.
type StateModule struct {
	Path      []string                  `json:"path"`
	Resources map[string]LegacyResource `json:"resources"`
}

// LegacyResource represents a resource in the legacy v0-v2 state format.
type LegacyResource struct {
	Type string `json:"type"`
}

// StateVersionInfo holds metadata about a specific state version.
type StateVersionInfo struct {
	DownloadURL string `json:"download_url"`
	Serial      int    `json:"serial"`
	CreatedAt   string `json:"created_at"`
}

// stateVersionResponse models the JSONAPI response for a single state version.
type stateVersionResponse struct {
	Data stateVersionData `json:"data"`
}

type stateVersionData struct {
	ID         string                 `json:"id"`
	Type       string                 `json:"type"`
	Attributes stateVersionAttributes `json:"attributes"`
}

type stateVersionAttributes struct {
	HostedStateDownloadURL string `json:"hosted-state-download-url"`
	Serial                 int    `json:"serial"`
	CreatedAt              string `json:"created-at"`
}

// DownloadState downloads and parses a Terraform state file from the given URL.
// The downloadURL is typically obtained from a state version's hosted-state-download-url
// attribute. It is already a fully-qualified URL (e.g. https://archivist.terraform.io/...)
// so we must NOT route it through httpClient.Get which would prepend the base URL.
func (c *Client) DownloadState(ctx context.Context, downloadURL string) (*StateFile, error) {
	if downloadURL == "" {
		return nil, fmt.Errorf("empty state download URL")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL, nil)
	if err != nil {
		return nil, fmt.Errorf("creating state download request: %w", err)
	}

	resp, err := c.httpClient.Do(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("downloading state file: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("unexpected status %d downloading state: %s", resp.StatusCode, string(bodyBytes))
	}

	var state StateFile
	if err := json.NewDecoder(resp.Body).Decode(&state); err != nil {
		return nil, fmt.Errorf("decoding state file JSON: %w", err)
	}

	return &state, nil
}

// DownloadStateRaw downloads a Terraform state file and returns both the raw
// JSON bytes and the parsed StateFile struct. This is useful for backups where
// the caller needs the original bytes for storage and the parsed metadata.
func (c *Client) DownloadStateRaw(ctx context.Context, downloadURL string) ([]byte, *StateFile, error) {
	if downloadURL == "" {
		return nil, nil, fmt.Errorf("empty state download URL")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("creating state download request: %w", err)
	}

	resp, err := c.httpClient.Do(ctx, req)
	if err != nil {
		return nil, nil, fmt.Errorf("downloading state file: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return nil, nil, fmt.Errorf("unexpected status %d downloading state: %s", resp.StatusCode, string(bodyBytes))
	}

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, fmt.Errorf("reading state file body: %w", err)
	}

	var state StateFile
	if err := json.Unmarshal(raw, &state); err != nil {
		return nil, nil, fmt.Errorf("decoding state file JSON: %w", err)
	}

	return raw, &state, nil
}

// GetCurrentStateVersion retrieves the current state version metadata for the
// given workspace ID. It calls GET {base_url}/workspaces/{id}/current-state-version.
func (c *Client) GetCurrentStateVersion(ctx context.Context, workspaceID string) (*StateVersionInfo, error) {
	path := fmt.Sprintf("/workspaces/%s/current-state-version", workspaceID)

	var resp stateVersionResponse
	if err := c.httpClient.GetJSON(ctx, path, nil, &resp); err != nil {
		return nil, fmt.Errorf("fetching current state version for workspace %s: %w", workspaceID, err)
	}

	return &StateVersionInfo{
		DownloadURL: resp.Data.Attributes.HostedStateDownloadURL,
		Serial:      resp.Data.Attributes.Serial,
		CreatedAt:   resp.Data.Attributes.CreatedAt,
	}, nil
}
