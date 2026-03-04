package hcp

import (
	"context"
	"fmt"
)

// Organization represents an HCP Terraform organization.
type Organization struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// organizationsResponse models the JSONAPI response for the organizations endpoint.
type organizationsResponse struct {
	Data []organizationData `json:"data"`
}

type organizationData struct {
	ID         string                 `json:"id"`
	Type       string                 `json:"type"`
	Attributes organizationAttributes `json:"attributes"`
}

type organizationAttributes struct {
	Name string `json:"name"`
}

// GetOrganizations returns all organizations accessible by the configured API token.
// It calls GET {base_url}/organizations and parses the JSONAPI response.
func (c *Client) GetOrganizations(ctx context.Context) ([]Organization, error) {
	var resp organizationsResponse
	if err := c.httpClient.GetJSON(ctx, "/organizations", nil, &resp); err != nil {
		return nil, fmt.Errorf("fetching organizations: %w", err)
	}

	orgs := make([]Organization, 0, len(resp.Data))
	for _, d := range resp.Data {
		orgs = append(orgs, Organization{
			ID:   d.ID,
			Name: d.Attributes.Name,
		})
	}

	return orgs, nil
}
