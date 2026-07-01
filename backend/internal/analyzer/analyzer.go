// Package analyzer parses Terraform state and computes the metrics the
// terraform-state-analyzer CLI produces: resource and instance counts, RUM
// (Resources Under Management), per-type / per-provider / per-module breakdowns,
// and Terraform version metadata. It is a Go port of that tool's core logic so
// the SPA can present the same analysis interactively.
//
// RUM follows HashiCorp's definition: managed resource instances, excluding
// null_resource and terraform_data; data sources are never counted. Instances
// are counted individually so count/for_each expansion is reflected.
package analyzer

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// AnalysisVersion is the revision of the analyzer's counting/normalization
// logic. Bump it whenever that logic changes so persisted analyses computed by
// an older revision are recognized as stale and recomputed — even when the
// underlying state bytes never changed. statesync folds this into its change
// marker, so a bump forces a one-time re-analysis of every stored state.
//
// History:
//
//	1: baseline — managed/data/RUM over the v4 resources[] shape only.
//	2: pre-v4 (0.11.x) legacy normalization + aliased-provider aggregation.
const AnalysisVersion = 2

// state mirrors the subset of the Terraform state we read. The top-level
// resources[]/instances[] shape is format v4 (Terraform 0.12+); the modules[]
// field captures the pre-v4 (Terraform 0.11.x) layout, which normalizeLegacy
// folds into Resources so the rest of the analyzer treats both uniformly.
type state struct {
	Version          int        `json:"version"`
	TerraformVersion string     `json:"terraform_version"`
	Serial           int64      `json:"serial"`
	Lineage          string     `json:"lineage"`
	Resources        []resource `json:"resources"`
	Modules          []v3Module `json:"modules"`
}

type resource struct {
	Module    string            `json:"module"`
	Mode      string            `json:"mode"` // "managed" or "data"
	Type      string            `json:"type"`
	Name      string            `json:"name"`
	Provider  string            `json:"provider"`
	Instances []json.RawMessage `json:"instances"`
}

// v3Module and v3Resource model the pre-v4 (Terraform 0.11.x) state layout, where
// resources live under modules[].resources as a map keyed by resource address and
// each entry carries a single primary instance plus any deposed instances, rather
// than v4's top-level resources[].instances[].
type v3Module struct {
	Path      []string              `json:"path"`
	Resources map[string]v3Resource `json:"resources"`
}

type v3Resource struct {
	Type     string            `json:"type"`
	Provider string            `json:"provider"`
	Primary  json.RawMessage   `json:"primary"`
	Deposed  []json.RawMessage `json:"deposed"`
}

// Count is a label/value pair used for sorted breakdowns in the API/UI.
type Count struct {
	Key   string `json:"key"`
	Count int    `json:"count"`
}

// Analysis is the computed summary of a single state file.
type Analysis struct {
	TerraformVersion string `json:"terraform_version"`
	FormatVersion    int    `json:"format_version"`
	Serial           int64  `json:"serial"`
	Lineage          string `json:"lineage"`

	TotalResources   int `json:"total_resources"`   // managed + data instances
	ManagedResources int `json:"managed_resources"` // managed instances
	DataSources      int `json:"data_sources"`      // data-source instances
	NullResources    int `json:"null_resources"`    // null_resource + terraform_data instances
	RUM              int `json:"rum"`               // managed - (null_resource + terraform_data)

	ResourceTypes []Count `json:"resource_types"` // by type, descending
	Providers     []Count `json:"providers"`      // by normalized provider, descending
	Modules       []Count `json:"modules"`        // by module path, descending
}

// Analyze parses raw Terraform state JSON and returns its analysis.
func Analyze(raw []byte) (*Analysis, error) {
	var s state
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("invalid Terraform state JSON: %w", err)
	}
	s.normalizeLegacy()
	if s.Version == 0 && s.Lineage == "" && len(s.Resources) == 0 {
		return nil, fmt.Errorf("input does not look like a Terraform state file")
	}

	a := &Analysis{
		TerraformVersion: s.TerraformVersion,
		FormatVersion:    s.Version,
		Serial:           s.Serial,
		Lineage:          s.Lineage,
	}

	types := map[string]int{}
	providers := map[string]int{}
	modules := map[string]int{}

	for _, r := range s.Resources {
		n := len(r.Instances)
		if n == 0 {
			continue
		}
		if r.Mode == "data" {
			a.DataSources += n
			continue
		}
		// managed
		a.ManagedResources += n
		types[r.Type] += n
		providers[normalizeProvider(r.Provider, r.Type)] += n
		modules[moduleLabel(r.Module)] += n
		if r.Type == "null_resource" || r.Type == "terraform_data" {
			a.NullResources += n
		}
	}

	a.TotalResources = a.ManagedResources + a.DataSources
	a.RUM = a.ManagedResources - a.NullResources
	a.ResourceTypes = sortedCounts(types)
	a.Providers = sortedCounts(providers)
	a.Modules = sortedCounts(modules)
	return a, nil
}

// normalizeProvider turns a state provider reference such as
// `provider["registry.terraform.io/hashicorp/aws"]` into `hashicorp/aws`. When
// the reference is missing it falls back to the resource type's leading segment
// (e.g. aws_instance → aws).
func normalizeProvider(provider, resourceType string) string {
	if provider != "" {
		ref := provider
		if i := strings.Index(ref, "[\""); i >= 0 {
			if j := strings.Index(ref[i+2:], "\""); j >= 0 {
				ref = ref[i+2 : i+2+j]
			}
		}
		ref = strings.TrimPrefix(ref, "provider.")
		// Drop the registry host, keeping <namespace>/<name>.
		parts := strings.Split(ref, "/")
		if len(parts) >= 3 {
			return strings.Join(parts[len(parts)-2:], "/")
		}
		if ref != "" {
			// A pre-v4 (0.11.x) ref is "<type>[.<alias>]" with no registry path;
			// drop the alias so aliased instances aggregate with the base provider.
			// (v4 refs carry a slash-bearing "<host>/<ns>/<name>" path, handled
			// above or left intact as "<ns>/<name>".)
			if !strings.Contains(ref, "/") {
				if i := strings.Index(ref, "."); i >= 0 {
					ref = ref[:i]
				}
			}
			return ref
		}
	}
	if i := strings.Index(resourceType, "_"); i > 0 {
		return resourceType[:i]
	}
	return "unknown"
}

func moduleLabel(module string) string {
	if module == "" {
		return "root"
	}
	return module
}

// normalizeLegacy folds a pre-v4 (Terraform 0.11.x) state's modules[].resources
// layout into the v4 resources[] shape so Analyze and ListResources can treat both
// formats uniformly. It is a no-op for v4 states, which already carry resources[]
// and no modules[].
func (s *state) normalizeLegacy() {
	if len(s.Resources) > 0 || len(s.Modules) == 0 {
		return
	}
	for _, m := range s.Modules {
		module := legacyModuleField(m.Path)
		// Map iteration order is non-deterministic; sort keys so the synthesized
		// resource order (and any tests over it) stays stable.
		keys := make([]string, 0, len(m.Resources))
		for k := range m.Resources {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, key := range keys {
			rv := m.Resources[key]
			rtype := rv.Type
			if rtype == "" {
				rtype = legacyTypeFromKey(key)
			}
			mode := "managed"
			if strings.HasPrefix(key, "data.") {
				mode = "data"
			}
			// Each v3 entry is one resource instance (count/for_each expansion uses
			// separate keys); deposed instances are still under management, so count
			// them alongside the primary.
			instances := make([]json.RawMessage, 0, 1+len(rv.Deposed))
			if len(rv.Primary) > 0 && string(rv.Primary) != "null" {
				instances = append(instances, rv.Primary)
			}
			instances = append(instances, rv.Deposed...)
			s.Resources = append(s.Resources, resource{
				Module:    module,
				Mode:      mode,
				Type:      rtype,
				Name:      legacyResourceName(key, rtype),
				Provider:  rv.Provider,
				Instances: instances,
			})
		}
	}
}

// legacyModuleField renders a pre-v4 module path (e.g. ["root","vpc"]) as the
// v4-style module address ("module.vpc"). The root module maps to "" to match the
// v4 convention that moduleLabel turns into "root".
func legacyModuleField(path []string) string {
	if len(path) <= 1 {
		return ""
	}
	parts := make([]string, 0, len(path)-1)
	for _, p := range path[1:] {
		parts = append(parts, "module."+p)
	}
	return strings.Join(parts, ".")
}

// legacyTypeFromKey extracts the resource type from a pre-v4 resource key such as
// "aws_instance.web", "aws_instance.web.0", or "data.aws_ami.ubuntu". Used only
// when the entry omits an explicit "type" field.
func legacyTypeFromKey(key string) string {
	key = strings.TrimPrefix(key, "data.")
	if i := strings.Index(key, "."); i > 0 {
		return key[:i]
	}
	return key
}

// legacyResourceName extracts the resource name (including any count index) from a
// pre-v4 resource key by stripping the optional "data." prefix and the leading
// "<type>." segment.
func legacyResourceName(key, rtype string) string {
	key = strings.TrimPrefix(key, "data.")
	return strings.TrimPrefix(key, rtype+".")
}

// sortedCounts returns counts sorted by descending count, then key ascending.
func sortedCounts(m map[string]int) []Count {
	out := make([]Count, 0, len(m))
	for k, v := range m {
		out = append(out, Count{Key: k, Count: v})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Key < out[j].Key
	})
	return out
}
