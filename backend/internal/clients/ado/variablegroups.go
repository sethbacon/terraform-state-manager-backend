package ado

import (
	"context"
	"fmt"
	"sort"
)

// VariableGroup represents an Azure DevOps variable group. Only variable names
// are captured; secret values are deliberately never read.
type VariableGroup struct {
	ID            int      `json:"id"`
	Name          string   `json:"name"`
	VariableNames []string `json:"variableNames"`
}

// variableGroupRaw models the raw distributedtask/variablegroups response item.
// The variables field maps a variable name to its definition; the definition
// (which may include a secret value) is decoded into discardedValue so that the
// value is never inspected or stored — only the map keys (names) are retained.
type variableGroupRaw struct {
	ID        int                       `json:"id"`
	Name      string                    `json:"name"`
	Variables map[string]discardedValue `json:"variables"`
}

// discardedValue is an empty struct used as the map value type so each variable
// definition (potentially a secret value) is parsed and immediately discarded.
type discardedValue struct{}

// ListVariableGroups returns all variable groups in the configured project,
// capturing only variable names. It calls
// GET {org}/{project}/_apis/distributedtask/variablegroups.
func (c *Client) ListVariableGroups(ctx context.Context) ([]VariableGroup, error) {
	var resp listEnvelope[variableGroupRaw]
	path := c.projectPath("_apis/distributedtask/variablegroups")
	if err := c.httpClient.GetJSON(ctx, path, defaultParams(), &resp); err != nil {
		return nil, fmt.Errorf("listing variable groups: %w", err)
	}

	groups := make([]VariableGroup, 0, len(resp.Value))
	for _, g := range resp.Value {
		names := make([]string, 0, len(g.Variables))
		for name := range g.Variables {
			names = append(names, name)
		}
		sort.Strings(names) // deterministic order; map iteration is random
		groups = append(groups, VariableGroup{
			ID:            g.ID,
			Name:          g.Name,
			VariableNames: names,
		})
	}
	return groups, nil
}

// variableValue is the wire representation of a single variable definition in a
// variable-group create body. Adopted variables carry empty placeholder values:
// the read client never reads secret values, so they are intentionally left
// blank to be re-supplied out of band rather than copied.
type variableValue struct {
	Value string `json:"value"`
}

// adoptVariableGroupBody is the wire body for POST
// _apis/distributedtask/variablegroups. The project reference scopes the new
// group to the target project.
type adoptVariableGroupBody struct {
	Name                           string                    `json:"name"`
	Type                           string                    `json:"type"`
	Variables                      map[string]variableValue  `json:"variables"`
	VariableGroupProjectReferences []variableGroupProjectRef `json:"variableGroupProjectReferences"`
}

type variableGroupProjectRef struct {
	Name             string             `json:"name"`
	ProjectReference projectReferenceID `json:"projectReference"`
}

// AdoptVariableGroup recreates a variable group named name in the configured
// (target) project, declaring the given variable names with empty placeholder
// values. Secret values are never copied — they must be re-supplied out of band.
// It calls POST {org}/_apis/distributedtask/variablegroups (project-scoped via
// the project reference) and returns the created group. A 409 Conflict is
// surfaced as an *APIError detectable via IsConflict so callers stay idempotent.
func (c *Client) AdoptVariableGroup(ctx context.Context, name string, variableNames []string) (*VariableGroup, error) {
	vars := make(map[string]variableValue, len(variableNames))
	for _, n := range variableNames {
		vars[n] = variableValue{Value: ""}
	}
	body := adoptVariableGroupBody{
		Name:      name,
		Type:      "Vsts",
		Variables: vars,
		VariableGroupProjectReferences: []variableGroupProjectRef{
			{
				Name:             name,
				ProjectReference: projectReferenceID{Name: c.config.Project},
			},
		},
	}
	var created variableGroupRaw
	// Variable groups are organization-scoped in API 7.1; the project reference
	// in the body scopes the group to the target project, so the path is not
	// project-prefixed.
	if err := c.postJSON(ctx, "_apis/distributedtask/variablegroups", defaultParams(), body, &created); err != nil {
		return nil, fmt.Errorf("adopting variable group %q: %w", name, err)
	}
	names := make([]string, 0, len(created.Variables))
	for n := range created.Variables {
		names = append(names, n)
	}
	sort.Strings(names)
	return &VariableGroup{
		ID:            created.ID,
		Name:          created.Name,
		VariableNames: names,
	}, nil
}
