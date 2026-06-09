package ado

import (
	"context"
	"encoding/json"
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

// AdoptBranchPolicyRequest describes a branch policy to recreate ("adopt") in
// the target project. TypeID is the policy type's GUID (as captured by
// ListBranchPolicies); Settings is the opaque policy settings object copied from
// the source (reviewer counts, scope, build definition id, etc.). The scope
// inside Settings must already reference target-project repository ids.
type AdoptBranchPolicyRequest struct {
	TypeID     string
	IsEnabled  bool
	IsBlocking bool
	Settings   json.RawMessage
}

// adoptBranchPolicyBody is the wire body for POST _apis/policy/configurations.
type adoptBranchPolicyBody struct {
	IsEnabled  bool            `json:"isEnabled"`
	IsBlocking bool            `json:"isBlocking"`
	Type       policyTypeRef   `json:"type"`
	Settings   json.RawMessage `json:"settings"`
}

type policyTypeRef struct {
	ID string `json:"id"`
}

// AdoptBranchPolicy recreates a branch policy in the configured (target)
// project. It calls POST {org}/{project}/_apis/policy/configurations and returns
// the created policy. A 409 Conflict (an equivalent policy already exists) is
// surfaced as an *APIError detectable via IsConflict so callers stay idempotent.
func (c *Client) AdoptBranchPolicy(ctx context.Context, req AdoptBranchPolicyRequest) (*BranchPolicy, error) {
	body := adoptBranchPolicyBody{
		IsEnabled:  req.IsEnabled,
		IsBlocking: req.IsBlocking,
		Type:       policyTypeRef{ID: req.TypeID},
		Settings:   req.Settings,
	}
	var created policyConfiguration
	path := c.projectPath("_apis/policy/configurations")
	if err := c.postJSON(ctx, path, defaultParams(), body, &created); err != nil {
		return nil, fmt.Errorf("adopting branch policy (type %s): %w", req.TypeID, err)
	}
	return &BranchPolicy{
		ID:          created.ID,
		Type:        created.Type.ID,
		DisplayName: created.Type.DisplayName,
		IsEnabled:   created.IsEnabled,
	}, nil
}
