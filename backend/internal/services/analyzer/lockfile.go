package analyzer

import (
	"fmt"
	"sort"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
)

// ProviderLockPin describes a single provider entry from a `.terraform.lock.hcl`
// dependency lock file: the resolved version and the version constraints that
// produced it.
type ProviderLockPin struct {
	// Source is the fully-qualified provider source address, e.g.
	// "registry.terraform.io/hashicorp/aws".
	Source string `json:"source"`
	// Version is the exact resolved version, e.g. "5.31.0".
	Version string `json:"version,omitempty"`
	// Constraints is the version constraint string, e.g. ">= 5.0.0, < 6.0.0".
	Constraints string `json:"constraints,omitempty"`
}

// ParseLockFile parses the HCL contents of a `.terraform.lock.hcl` dependency
// lock file and returns the provider pins keyed by provider source address.
//
// Only `provider "<source>" { ... }` blocks are inspected; the `version` and
// `constraints` attributes are extracted and `hashes` is ignored. A parse error
// is returned only when the input is not valid HCL.
func ParseLockFile(src []byte) (map[string]ProviderLockPin, error) {
	file, diags := hclsyntax.ParseConfig(src, ".terraform.lock.hcl", hcl.InitialPos)
	if diags.HasErrors() {
		return nil, fmt.Errorf("failed to parse lock file: %s", diags.Error())
	}

	body, ok := file.Body.(*hclsyntax.Body)
	if !ok {
		return nil, fmt.Errorf("unexpected HCL body type for lock file")
	}

	pins := make(map[string]ProviderLockPin)

	for _, block := range body.Blocks {
		if block.Type != "provider" || len(block.Labels) == 0 {
			continue
		}
		source := block.Labels[0]
		pin := ProviderLockPin{Source: source}
		if v, ok := stringAttr(block.Body, "version"); ok {
			pin.Version = v
		}
		if v, ok := stringAttr(block.Body, "constraints"); ok {
			pin.Constraints = v
		}
		pins[source] = pin
	}

	return pins, nil
}

// SortedLockPins returns the provider pins as a slice sorted by source address,
// for deterministic JSON output and stable test assertions.
func SortedLockPins(pins map[string]ProviderLockPin) []ProviderLockPin {
	out := make([]ProviderLockPin, 0, len(pins))
	for _, pin := range pins {
		out = append(out, pin)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Source < out[j].Source
	})
	return out
}
