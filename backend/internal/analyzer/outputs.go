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

// stateOutputs mirrors the top-level "outputs" map of state format v4. The
// type field is a cty JSON type: either a primitive name string or an array
// like ["list","string"] / ["object",{...}].
type stateOutputs struct {
	Version   int                    `json:"version"`
	Lineage   string                 `json:"lineage"`
	Outputs   map[string]stateOutput `json:"outputs"`
	Resources []json.RawMessage      `json:"resources"`
}

type stateOutput struct {
	Value     json.RawMessage `json:"value"`
	Type      json.RawMessage `json:"type"`
	Sensitive bool            `json:"sensitive"`
}

// ListOutputs returns the root-module outputs for the State Detail "Outputs"
// view, sorted by name, with sensitive values redacted.
func ListOutputs(raw []byte) ([]OutputSummary, error) {
	var s stateOutputs
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("invalid Terraform state JSON: %w", err)
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
