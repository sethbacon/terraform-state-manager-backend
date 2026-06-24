package analyzer

import (
	"encoding/json"
	"fmt"
	"sort"
)

// OutputSummary describes one root-module output in the state. Sensitive
// output values are redacted here, at the source, so they can never reach an
// API response or the UI.
type OutputSummary struct {
	Name      string          `json:"name"`
	Type      string          `json:"type"`
	Sensitive bool            `json:"sensitive"`
	Value     json.RawMessage `json:"value,omitempty"` // omitted when sensitive
}

// stateOutputs mirrors the "outputs" map of a Terraform state. In format v4
// outputs live at the top level; in pre-v4 (Terraform 0.11.x) states they live
// under modules[].outputs, which ListOutputs folds in from the root module. The
// type field is a cty JSON type — a primitive name string ("string") or an array
// like ["list","string"] / ["object",{...}] — and v3's plain "string"/"list"/"map"
// labels parse through the same primitive path.
type stateOutputs struct {
	Version   int                    `json:"version"`
	Lineage   string                 `json:"lineage"`
	Outputs   map[string]stateOutput `json:"outputs"`
	Resources []json.RawMessage      `json:"resources"`
	Modules   []legacyOutputModule   `json:"modules"`
}

type stateOutput struct {
	Value     json.RawMessage `json:"value"`
	Type      json.RawMessage `json:"type"`
	Sensitive bool            `json:"sensitive"`
}

// legacyOutputModule captures a pre-v4 (Terraform 0.11.x) module's outputs; only
// the root module's outputs are surfaced by ListOutputs.
type legacyOutputModule struct {
	Path    []string               `json:"path"`
	Outputs map[string]stateOutput `json:"outputs"`
}

// ListOutputs returns the root-module outputs for the State Detail "Outputs"
// view, sorted by name, with sensitive values redacted.
func ListOutputs(raw []byte) ([]OutputSummary, error) {
	var s stateOutputs
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("invalid Terraform state JSON: %w", err)
	}
	// Pre-v4 states keep outputs under the root module rather than at the top
	// level; fold those in so both formats flow through the same logic below.
	if len(s.Outputs) == 0 {
		if legacy := rootModuleOutputs(s.Modules); len(legacy) > 0 {
			s.Outputs = legacy
		}
	}
	if s.Version == 0 && s.Lineage == "" && len(s.Resources) == 0 && len(s.Outputs) == 0 {
		return nil, fmt.Errorf("input does not look like a Terraform state file")
	}

	out := make([]OutputSummary, 0, len(s.Outputs))
	for name, o := range s.Outputs {
		entry := OutputSummary{Name: name, Type: ctyTypeLabel(o.Type), Sensitive: o.Sensitive}
		if !o.Sensitive {
			entry.Value = o.Value
		}
		out = append(out, entry)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// rootModuleOutputs returns the outputs of the pre-v4 root module (path ["root"]),
// or nil when absent. v3 (Terraform 0.11.x) states store outputs under
// modules[].outputs instead of at the top level.
func rootModuleOutputs(modules []legacyOutputModule) map[string]stateOutput {
	for _, m := range modules {
		if len(m.Path) == 1 && m.Path[0] == "root" {
			return m.Outputs
		}
	}
	return nil
}

// ctyTypeLabel renders a cty JSON type compactly: primitives as-is ("string"),
// containers by their kind ("list", "object", ...).
func ctyTypeLabel(t json.RawMessage) string {
	if len(t) == 0 {
		return ""
	}
	var prim string
	if err := json.Unmarshal(t, &prim); err == nil {
		return prim
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(t, &arr); err == nil && len(arr) > 0 {
		var kind string
		if err := json.Unmarshal(arr[0], &kind); err == nil {
			return kind
		}
	}
	return "complex"
}
