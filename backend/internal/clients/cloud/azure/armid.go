package azure

import (
	"fmt"
	"strings"
)

// ResourceID is the parsed form of an Azure Resource Manager (ARM) resource ID,
// the value Terraform's azurerm provider stores in a resource instance's id
// attribute. A canonical ARM ID has the shape:
//
//	/subscriptions/{sub}/resourceGroups/{rg}/providers/{namespace}/{type}/{name}
//
// possibly with further /{childType}/{childName} segments for nested resources
// (for example a subnet under a virtual network). ParseResourceID captures the
// pieces needed to issue a read-only ARM GET and to label drift findings.
type ResourceID struct {
	// Raw is the original, unmodified ARM resource ID.
	Raw string
	// SubscriptionID is the GUID following /subscriptions/.
	SubscriptionID string
	// ResourceGroup is the name following /resourceGroups/. It may be empty for
	// subscription- or tenant-scoped resources.
	ResourceGroup string
	// Namespace is the provider namespace following /providers/, e.g.
	// "Microsoft.Network".
	Namespace string
	// Type is the full, possibly nested, resource type path relative to the
	// namespace, e.g. "virtualNetworks" or "virtualNetworks/subnets".
	Type string
	// Name is the resource name. For nested resources it is the final name
	// segment, e.g. the subnet name.
	Name string
}

// FullType returns the namespace-qualified resource type, e.g.
// "Microsoft.Network/virtualNetworks". It is the key used to look up an ARM API
// version for the GET request.
func (id ResourceID) FullType() string {
	return id.Namespace + "/" + id.Type
}

// ParseResourceID parses an ARM resource ID into its components. It is tolerant
// of a leading or trailing slash and of mixed-case segment keywords (Azure
// treats the /subscriptions, /resourceGroups, /providers keywords
// case-insensitively). It returns an error if the ID is not a recognisable ARM
// resource ID — for example a non-azurerm resource whose id attribute is an
// opaque string rather than an ARM path.
func ParseResourceID(raw string) (ResourceID, error) {
	trimmed := strings.Trim(strings.TrimSpace(raw), "/")
	if trimmed == "" {
		return ResourceID{}, fmt.Errorf("azure: empty resource ID")
	}

	segments := strings.Split(trimmed, "/")
	id := ResourceID{Raw: raw}

	// Walk the leading scope segments (subscriptions, resourceGroups) until the
	// providers keyword, which begins the resource path proper.
	i := 0
	for i < len(segments) {
		key := strings.ToLower(segments[i])
		if key == "providers" {
			break
		}
		// Each scope keyword is followed by its value.
		if i+1 >= len(segments) {
			return ResourceID{}, fmt.Errorf("azure: malformed resource ID %q: dangling segment %q", raw, segments[i])
		}
		value := segments[i+1]
		switch key {
		case "subscriptions":
			id.SubscriptionID = value
		case "resourcegroups":
			id.ResourceGroup = value
		}
		i += 2
	}

	if i >= len(segments) || strings.ToLower(segments[i]) != "providers" {
		return ResourceID{}, fmt.Errorf("azure: not an ARM resource ID (no provider segment): %q", raw)
	}
	i++ // consume the "providers" keyword.

	if i >= len(segments) {
		return ResourceID{}, fmt.Errorf("azure: malformed resource ID %q: missing provider namespace", raw)
	}
	id.Namespace = segments[i]
	i++

	// The remaining segments are alternating type/name pairs. A valid resource
	// path has an even number of remaining segments and at least one pair.
	rest := segments[i:]
	if len(rest) == 0 || len(rest)%2 != 0 {
		return ResourceID{}, fmt.Errorf("azure: malformed resource ID %q: unbalanced type/name path", raw)
	}

	typeParts := make([]string, 0, len(rest)/2)
	for j := 0; j < len(rest); j += 2 {
		typeParts = append(typeParts, rest[j])
		id.Name = rest[j+1]
	}
	id.Type = strings.Join(typeParts, "/")

	if id.Namespace == "" || id.Type == "" || id.Name == "" {
		return ResourceID{}, fmt.Errorf("azure: malformed resource ID %q: incomplete resource path", raw)
	}

	return id, nil
}
