package hcp

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
)

// Workspace represents an HCP Terraform workspace with its current state metadata.
type Workspace struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	Organization     string `json:"organization"`
	TerraformVersion string `json:"terraform_version"`
	CreatedAt        string `json:"created_at"`
	UpdatedAt        string `json:"updated_at"`
	StateVersionID   string `json:"state_version_id"`
	StateDownloadURL string `json:"state_download_url"`
	StateSerial      int    `json:"state_serial"`
}

// workspacesResponse models the paginated JSONAPI response for workspace listings.
type workspacesResponse struct {
	Data     []workspaceData    `json:"data"`
	Included []includedResource `json:"included"`
	Meta     paginationMeta     `json:"meta"`
}

type workspaceData struct {
	ID            string              `json:"id"`
	Type          string              `json:"type"`
	Attributes    workspaceAttributes `json:"attributes"`
	Relationships workspaceRelations  `json:"relationships"`
}

type workspaceAttributes struct {
	Name             string `json:"name"`
	TerraformVersion string `json:"terraform-version"`
	CreatedAt        string `json:"created-at"`
	UpdatedAt        string `json:"updated-at"`
}

type workspaceRelations struct {
	Organization        relationshipData `json:"organization"`
	CurrentStateVersion relationshipData `json:"current-state-version"`
}

type relationshipData struct {
	Data *relationshipRef `json:"data"`
}

type relationshipRef struct {
	ID   string `json:"id"`
	Type string `json:"type"`
}

type includedResource struct {
	ID         string                 `json:"id"`
	Type       string                 `json:"type"`
	Attributes map[string]interface{} `json:"attributes"`
}

type paginationMeta struct {
	Pagination pagination `json:"pagination"`
}

type pagination struct {
	CurrentPage int `json:"current-page"`
	TotalPages  int `json:"total-pages"`
}

// GetWorkspaces fetches all workspaces for the given organization, handling
// pagination automatically. It includes the current state version to extract
// state download URLs and serial numbers.
func (c *Client) GetWorkspaces(ctx context.Context, orgName string) ([]Workspace, error) {
	var allWorkspaces []Workspace
	page := 1

	for {
		params := url.Values{}
		params.Set("page[size]", strconv.Itoa(c.config.BatchSize))
		params.Set("page[number]", strconv.Itoa(page))
		params.Set("include", "current-state-version")

		path := fmt.Sprintf("/organizations/%s/workspaces", url.PathEscape(orgName))

		var resp workspacesResponse
		if err := c.httpClient.GetJSON(ctx, path, params, &resp); err != nil {
			return nil, fmt.Errorf("fetching workspaces for org %s (page %d): %w", orgName, page, err)
		}

		// Build a lookup map from included state versions
		stateVersions := buildStateVersionLookup(resp.Included)

		for _, ws := range resp.Data {
			workspace := Workspace{
				ID:               ws.ID,
				Name:             ws.Attributes.Name,
				Organization:     orgName,
				TerraformVersion: ws.Attributes.TerraformVersion,
				CreatedAt:        ws.Attributes.CreatedAt,
				UpdatedAt:        ws.Attributes.UpdatedAt,
			}

			// Link state version data from included resources
			if ws.Relationships.CurrentStateVersion.Data != nil {
				svID := ws.Relationships.CurrentStateVersion.Data.ID
				workspace.StateVersionID = svID
				if sv, ok := stateVersions[svID]; ok {
					workspace.StateDownloadURL = sv.DownloadURL
					workspace.StateSerial = sv.Serial
				}
			}

			allWorkspaces = append(allWorkspaces, workspace)
		}

		if page >= resp.Meta.Pagination.TotalPages {
			break
		}
		page++
	}

	return allWorkspaces, nil
}

// stateVersionInfo holds parsed data from an included state-version resource.
type stateVersionLookupEntry struct {
	DownloadURL string
	Serial      int
}

// buildStateVersionLookup creates a map from state version ID to its download
// URL and serial number, extracted from JSONAPI included resources.
func buildStateVersionLookup(included []includedResource) map[string]stateVersionLookupEntry {
	result := make(map[string]stateVersionLookupEntry)
	for _, inc := range included {
		if inc.Type != "state-versions" {
			continue
		}
		entry := stateVersionLookupEntry{}

		if dl, ok := inc.Attributes["hosted-state-download-url"]; ok {
			if dlStr, ok := dl.(string); ok {
				entry.DownloadURL = dlStr
			}
		}
		if serial, ok := inc.Attributes["serial"]; ok {
			switch v := serial.(type) {
			case float64:
				entry.Serial = int(v)
			case int:
				entry.Serial = v
			}
		}

		result[inc.ID] = entry
	}
	return result
}

// GetAllWorkspaces fetches workspaces across all accessible organizations.
// If orgFilter is non-empty, only workspaces from that organization are returned.
func (c *Client) GetAllWorkspaces(ctx context.Context, orgFilter string) ([]Workspace, error) {
	if orgFilter != "" {
		return c.GetWorkspaces(ctx, orgFilter)
	}

	orgs, err := c.GetOrganizations(ctx)
	if err != nil {
		return nil, fmt.Errorf("fetching organizations for workspace listing: %w", err)
	}

	var allWorkspaces []Workspace
	for _, org := range orgs {
		workspaces, err := c.GetWorkspaces(ctx, org.Name)
		if err != nil {
			return nil, fmt.Errorf("fetching workspaces for org %s: %w", org.Name, err)
		}
		allWorkspaces = append(allWorkspaces, workspaces...)
	}

	return allWorkspaces, nil
}
