package analyzer

import (
	"encoding/json"
	"fmt"
	"sort"
)

// ResourceSummary describes one resource block in the state and how many
// instances it expanded to (count/for_each).
type ResourceSummary struct {
	Module    string `json:"module"`
	Mode      string `json:"mode"` // managed | data
	Type      string `json:"type"`
	Name      string `json:"name"`
	Provider  string `json:"provider"`
	Instances int    `json:"instances"`
	// InstanceKeys lists each instance's index_key (for_each string keys or count
	// integers) so callers can target a single instance for rm/mv. It is omitted
	// for un-indexed singleton resources (a lone instance with no index_key).
	InstanceKeys []any `json:"instance_keys,omitempty"`
}

// ListResources returns a flat, sorted list of the resources in the state for the
// State Detail "Resources" view.
func ListResources(raw []byte) ([]ResourceSummary, error) {
	var s state
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("invalid Terraform state JSON: %w", err)
	}
	s.normalizeLegacy()
	out := make([]ResourceSummary, 0, len(s.Resources))
	for _, r := range s.Resources {
		out = append(out, ResourceSummary{
			Module:       moduleLabel(r.Module),
			Mode:         r.Mode,
			Type:         r.Type,
			Name:         r.Name,
			Provider:     normalizeProvider(r.Provider, r.Type),
			Instances:    len(r.Instances),
			InstanceKeys: instanceKeys(r.Instances),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Module != out[j].Module {
			return out[i].Module < out[j].Module
		}
		if out[i].Type != out[j].Type {
			return out[i].Type < out[j].Type
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// instanceKeys returns the index_key of each instance that has one (for_each
// string keys or count integers), preserving state order. It returns nil for an
// un-indexed singleton resource, where there is no distinct instance to target.
func instanceKeys(instances []json.RawMessage) []any {
	keys := make([]any, 0, len(instances))
	for _, raw := range instances {
		var inst struct {
			IndexKey any `json:"index_key"`
		}
		if err := json.Unmarshal(raw, &inst); err != nil || inst.IndexKey == nil {
			continue
		}
		keys = append(keys, inst.IndexKey)
	}
	if len(keys) == 0 {
		return nil
	}
	return keys
}
