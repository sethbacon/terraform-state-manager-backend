// Package envdrift compares the azurerm resources recorded in a Terraform state
// against their live counterparts in Azure and records the differences as
// environment drift. For each managed azurerm resource it looks up the live ARM
// resource by the ARM resource ID stored in the state's id attribute and
// classifies it as present, missing, or changed. The aggregated result is
// written as a drift_events row with drift_source = "environment".
//
// This slice is the engine only: it exposes a single entry point,
// Service.DetectForState, that the scheduler or an HTTP trigger will call in a
// later slice. It contains no scheduling and no HTTP wiring.
package envdrift

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/terraform-state-manager/terraform-state-manager/internal/clients/hcp"
)

// azureProviderMarker identifies azurerm-provider resources by their provider
// string, which looks like provider["registry.terraform.io/hashicorp/azurerm"].
const azureProviderMarker = "/azurerm"

// StateResourceRef identifies a single azurerm resource instance pulled from a
// Terraform state: its address-style address and the ARM resource ID used to
// look it up in Azure.
type StateResourceRef struct {
	// Address is the Terraform address of the resource, e.g.
	// "azurerm_virtual_network.main". Used only for labelling drift findings.
	Address string
	// Type is the Terraform resource type, e.g. "azurerm_virtual_network".
	Type string
	// ARMID is the value of the instance's id attribute — the ARM resource ID.
	ARMID string
}

// instanceAttributes is the subset of a state instance's attributes we read.
type instanceAttributes struct {
	ID string `json:"id"`
}

// ExtractAzureResources returns the azurerm resource instances in state that
// carry a non-empty ARM resource ID. Data sources (mode == "data") are skipped
// because they describe lookups, not managed infrastructure. Resources whose id
// attribute is empty or missing are skipped: without an ARM ID there is nothing
// to look up. The returned refs preserve state order for deterministic results.
func ExtractAzureResources(state *hcp.StateFile) []StateResourceRef {
	if state == nil {
		return nil
	}

	refs := make([]StateResourceRef, 0)
	for _, res := range state.Resources {
		if !isAzureResource(res) {
			continue
		}
		if res.Mode == "data" {
			continue
		}
		for idx, inst := range res.Instances {
			var attrs instanceAttributes
			if len(inst.Attributes) == 0 {
				continue
			}
			if err := json.Unmarshal(inst.Attributes, &attrs); err != nil {
				continue
			}
			if attrs.ID == "" {
				continue
			}
			refs = append(refs, StateResourceRef{
				Address: resourceAddress(res, idx),
				Type:    res.Type,
				ARMID:   attrs.ID,
			})
		}
	}
	return refs
}

// isAzureResource reports whether res belongs to the azurerm provider, detected
// via either its provider string or its azurerm_ type prefix.
func isAzureResource(res hcp.StateResource) bool {
	if strings.Contains(res.Provider, azureProviderMarker) {
		return true
	}
	return strings.HasPrefix(res.Type, "azurerm_")
}

// resourceAddress builds a Terraform-style address for a resource instance,
// appending an index suffix when the resource has multiple instances (count or
// for_each), e.g. "azurerm_subnet.main[1]".
func resourceAddress(res hcp.StateResource, idx int) string {
	base := res.Type + "." + res.Name
	if res.Module != "" {
		base = res.Module + "." + base
	}
	if len(res.Instances) > 1 {
		base += "[" + strconv.Itoa(idx) + "]"
	}
	return base
}
