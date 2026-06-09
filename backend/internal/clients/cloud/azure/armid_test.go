package azure

import "testing"

func TestParseResourceID(t *testing.T) {
	tests := []struct {
		name        string
		raw         string
		wantSub     string
		wantRG      string
		wantNS      string
		wantType    string
		wantName    string
		wantFull    string
		expectError bool
	}{
		{
			name:     "virtual network",
			raw:      "/subscriptions/sub-1/resourceGroups/rg-platform/providers/Microsoft.Network/virtualNetworks/vnet-main",
			wantSub:  "sub-1",
			wantRG:   "rg-platform",
			wantNS:   "Microsoft.Network",
			wantType: "virtualNetworks",
			wantName: "vnet-main",
			wantFull: "Microsoft.Network/virtualNetworks",
		},
		{
			name:     "nested subnet",
			raw:      "/subscriptions/sub-1/resourceGroups/rg-platform/providers/Microsoft.Network/virtualNetworks/vnet-main/subnets/app",
			wantSub:  "sub-1",
			wantRG:   "rg-platform",
			wantNS:   "Microsoft.Network",
			wantType: "virtualNetworks/subnets",
			wantName: "app",
			wantFull: "Microsoft.Network/virtualNetworks/subnets",
		},
		{
			name:     "case-insensitive keywords",
			raw:      "/Subscriptions/sub-1/ResourceGroups/rg-x/Providers/Microsoft.Storage/storageAccounts/acct",
			wantSub:  "sub-1",
			wantRG:   "rg-x",
			wantNS:   "Microsoft.Storage",
			wantType: "storageAccounts",
			wantName: "acct",
			wantFull: "Microsoft.Storage/storageAccounts",
		},
		{name: "empty", raw: "", expectError: true},
		{name: "opaque non-arm id", raw: "some-opaque-resource-id", expectError: true},
		{name: "no providers segment", raw: "/subscriptions/sub-1/resourceGroups/rg-x", expectError: true},
		{
			name:        "unbalanced type path",
			raw:         "/subscriptions/sub-1/resourceGroups/rg-x/providers/Microsoft.Network/virtualNetworks",
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id, err := ParseResourceID(tt.raw)
			if tt.expectError {
				if err == nil {
					t.Fatalf("expected error for %q, got none", tt.raw)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if id.SubscriptionID != tt.wantSub {
				t.Errorf("SubscriptionID = %q, want %q", id.SubscriptionID, tt.wantSub)
			}
			if id.ResourceGroup != tt.wantRG {
				t.Errorf("ResourceGroup = %q, want %q", id.ResourceGroup, tt.wantRG)
			}
			if id.Namespace != tt.wantNS {
				t.Errorf("Namespace = %q, want %q", id.Namespace, tt.wantNS)
			}
			if id.Type != tt.wantType {
				t.Errorf("Type = %q, want %q", id.Type, tt.wantType)
			}
			if id.Name != tt.wantName {
				t.Errorf("Name = %q, want %q", id.Name, tt.wantName)
			}
			if id.FullType() != tt.wantFull {
				t.Errorf("FullType() = %q, want %q", id.FullType(), tt.wantFull)
			}
		})
	}
}

func TestAPIVersionFor(t *testing.T) {
	if got := apiVersionFor("Microsoft.Network/virtualNetworks"); got != "2023-09-01" {
		t.Errorf("known type api-version = %q, want 2023-09-01", got)
	}
	// Case-insensitive lookup.
	if got := apiVersionFor("microsoft.storage/storageAccounts"); got != "2023-01-01" {
		t.Errorf("case-insensitive api-version = %q, want 2023-01-01", got)
	}
	// Unknown type falls back.
	if got := apiVersionFor("Microsoft.Made/upType"); got != fallbackAPIVersion {
		t.Errorf("unknown type api-version = %q, want %q", got, fallbackAPIVersion)
	}
}
