package azure

import "encoding/json"

// keyPropertyNames are the property keys captured for environment-drift
// comparison. They are stable, low-cardinality fields that change when a
// resource is reconfigured out-of-band (for example a VM resized or a storage
// account's SKU changed), without pulling in the full, noisy resource body.
const (
	PropLocation          = "location"
	PropKind              = "kind"
	PropSKU               = "sku"
	PropProvisioningState = "provisioning_state"
)

// ExtractKeyProperties builds the comparable property map for a present
// resource from its top-level location/kind/sku and the provisioningState found
// inside the ARM properties bag. Empty values are omitted so a resource that
// simply does not expose a field never registers as drift against one that
// does. It returns nil when no key properties are available.
func ExtractKeyProperties(location, kind, sku string, properties json.RawMessage) map[string]string {
	out := make(map[string]string, 4)
	if location != "" {
		out[PropLocation] = location
	}
	if kind != "" {
		out[PropKind] = kind
	}
	if sku != "" {
		out[PropSKU] = sku
	}
	if ps := provisioningState(properties); ps != "" {
		out[PropProvisioningState] = ps
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// provisioningState extracts the provisioningState string from an ARM
// properties bag, returning "" if absent or unparseable.
func provisioningState(properties json.RawMessage) string {
	if len(properties) == 0 {
		return ""
	}
	var p struct {
		ProvisioningState string `json:"provisioningState"`
	}
	if err := json.Unmarshal(properties, &p); err != nil {
		return ""
	}
	return p.ProvisioningState
}
