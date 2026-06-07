package ado

import (
	"context"
	"fmt"
)

// BranchPolicy represents an Azure DevOps branch policy configuration.
type BranchPolicy struct {
	ID          int    `json:"id"`
	Type        string `json:"type"`
	DisplayName string `json:"displayName"`
	IsEnabled   bool   `json:"isEnabled"`
}

// policyConfiguration models the raw policy/configurations response item, where
// the policy type is a nested object carrying both an identifier and a human
// readable display name.
type policyConfiguration struct {
	ID        int  `json:"id"`
	IsEnabled bool `json:"isEnabled"`
	Type      struct {
		ID          string `json:"id"`
		DisplayName string `json:"displayName"`
	} `json:"type"`
}

// ListBranchPolicies returns all policy configurations in the configured
// project. It calls GET {org}/{project}/_apis/policy/configurations.
func (c *Client) ListBranchPolicies(ctx context.Context) ([]BranchPolicy, error) {
	var resp listEnvelope[policyConfiguration]
	path := c.projectPath("_apis/policy/configurations")
	if err := c.httpClient.GetJSON(ctx, path, defaultParams(), &resp); err != nil {
		return nil, fmt.Errorf("listing branch policies: %w", err)
	}

	policies := make([]BranchPolicy, 0, len(resp.Value))
	for _, p := range resp.Value {
		policies = append(policies, BranchPolicy{
			ID:          p.ID,
			Type:        p.Type.ID,
			DisplayName: p.Type.DisplayName,
			IsEnabled:   p.IsEnabled,
		})
	}
	return policies, nil
}
