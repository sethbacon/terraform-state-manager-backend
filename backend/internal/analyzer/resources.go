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
			Module:    moduleLabel(r.Module),
			Mode:      r.Mode,
			Type:      r.Type,
			Name:      r.Name,
			Provider:  normalizeProvider(r.Provider, r.Type),
			Instances: len(r.Instances),
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
