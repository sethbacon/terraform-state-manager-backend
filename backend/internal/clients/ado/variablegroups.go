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
