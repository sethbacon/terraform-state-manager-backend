package hcp

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
)

// ProviderInfo represents a Terraform provider as reported by the HCP Explorer API,
// including usage statistics across workspaces.
type ProviderInfo struct {
	Name           string `json:"name"`
	Source         string `json:"source"`
	Version        string `json:"version"`
	WorkspaceCount int    `json:"workspace_count"`
	Workspaces     string `json:"workspaces"`
}

// explorerResponse models the paginated JSONAPI response from the explorer endpoint.
type explorerResponse struct {
	Data []explorerData `json:"data"`
	Meta paginationMeta `json:"meta"`
}

type explorerData struct {
	ID         string             `json:"id"`
	Type       string             `json:"type"`
	Attributes explorerAttributes `json:"attributes"`
}

type explorerAttributes struct {
	Name           string `json:"name"`
	Source         string `json:"source"`
	Version        string `json:"version"`
	WorkspaceCount int    `json:"workspace-count"`
	Workspaces     string `json:"workspaces"`
}

// GetProviderInfo fetches all providers used across workspaces in the given
// organization, sorted by workspace count in descending order. It handles
// pagination automatically, collecting results from all pages.
func (c *Client) GetProviderInfo(ctx context.Context, orgName string) ([]ProviderInfo, error) {
	var allProviders []ProviderInfo
	page := 1

	for {
		params := url.Values{}
		params.Set("type", "providers")
		params.Set("sort", "-workspace_count")
		params.Set("page[size]", strconv.Itoa(c.config.BatchSize))
		params.Set("page[number]", strconv.Itoa(page))

		path := fmt.Sprintf("/organizations/%s/explorer", url.PathEscape(orgName))

		var resp explorerResponse
		if err := c.httpClient.GetJSON(ctx, path, params, &resp); err != nil {
			return nil, fmt.Errorf("fetching provider info for org %s (page %d): %w", orgName, page, err)
		}

		for _, d := range resp.Data {
			allProviders = append(allProviders, ProviderInfo{
				Name:           d.Attributes.Name,
				Source:         d.Attributes.Source,
				Version:        d.Attributes.Version,
				WorkspaceCount: d.Attributes.WorkspaceCount,
				Workspaces:     d.Attributes.Workspaces,
			})
		}

		if page >= resp.Meta.Pagination.TotalPages || resp.Meta.Pagination.TotalPages == 0 {
			break
		}
		page++
	}

	return allProviders, nil
}
