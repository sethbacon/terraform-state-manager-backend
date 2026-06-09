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

// state mirrors the subset of the Terraform state (format v4) we read.
type state struct {
	Version          int        `json:"version"`
	TerraformVersion string     `json:"terraform_version"`
	Serial           int64      `json:"serial"`
	Lineage          string     `json:"lineage"`
	Resources        []resource `json:"resources"`
}

type resource struct {
	Module    string            `json:"module"`
	Mode      string            `json:"mode"` // "managed" or "data"
	Type      string            `json:"type"`
	Name      string            `json:"name"`
	Provider  string            `json:"provider"`
	Instances []json.RawMessage `json:"instances"`
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
