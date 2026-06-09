package envdrift

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/terraform-state-manager/terraform-state-manager/internal/clients/hcp"
)

func loadState(t *testing.T) *hcp.StateFile {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "state.json"))
	if err != nil {
		t.Fatalf("reading state fixture: %v", err)
	}
	var state hcp.StateFile
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatalf("unmarshalling state fixture: %v", err)
	}
	return &state
}

func TestExtractAzureResources(t *testing.T) {
	state := loadState(t)
	refs := ExtractAzureResources(state)

	// Three managed azurerm resources with ARM IDs; the data source and the
	// non-azure random_id resource are excluded.
	if len(refs) != 3 {
		t.Fatalf("got %d refs, want 3: %+v", len(refs), refs)
	}

	byType := map[string]StateResourceRef{}
	for _, r := range refs {
		byType[r.Type] = r
	}

	vnet, ok := byType["azurerm_virtual_network"]
	if !ok {
		t.Fatal("missing azurerm_virtual_network ref")
	}
	if vnet.Address != "azurerm_virtual_network.main" {
		t.Errorf("vnet Address = %q", vnet.Address)
	}
	if vnet.ARMID == "" {
		t.Error("vnet ARMID is empty")
	}

	if _, ok := byType["azurerm_client_config"]; ok {
		t.Error("data source azurerm_client_config should be excluded")
	}
	if _, ok := byType["random_id"]; ok {
		t.Error("non-azure random_id should be excluded")
	}
}

func TestExtractAzureResources_NilState(t *testing.T) {
	if refs := ExtractAzureResources(nil); refs != nil {
		t.Errorf("expected nil for nil state, got %v", refs)
	}
}

func TestExtractAzureResources_SkipsEmptyID(t *testing.T) {
	state := &hcp.StateFile{
		Resources: []hcp.StateResource{
			{
				Mode:     "managed",
				Type:     "azurerm_resource_group",
				Name:     "noid",
				Provider: "provider[\"registry.terraform.io/hashicorp/azurerm\"]",
				Instances: []hcp.StateInstance{
					{Attributes: json.RawMessage(`{"location":"eastus"}`)},
				},
			},
		},
	}
	if refs := ExtractAzureResources(state); len(refs) != 0 {
		t.Errorf("expected no refs for resource without id, got %v", refs)
	}
}

func TestResourceAddress_MultiInstanceIndexed(t *testing.T) {
	state := &hcp.StateFile{
		Resources: []hcp.StateResource{
			{
				Mode:     "managed",
				Type:     "azurerm_subnet",
				Name:     "app",
				Provider: "provider[\"registry.terraform.io/hashicorp/azurerm\"]",
				Instances: []hcp.StateInstance{
					{Attributes: json.RawMessage(`{"id":"/subscriptions/s/resourceGroups/r/providers/Microsoft.Network/virtualNetworks/v/subnets/a"}`)},
					{Attributes: json.RawMessage(`{"id":"/subscriptions/s/resourceGroups/r/providers/Microsoft.Network/virtualNetworks/v/subnets/b"}`)},
				},
			},
		},
	}
	refs := ExtractAzureResources(state)
	if len(refs) != 2 {
		t.Fatalf("got %d refs, want 2", len(refs))
	}
	if refs[0].Address != "azurerm_subnet.app[0]" {
		t.Errorf("refs[0].Address = %q, want azurerm_subnet.app[0]", refs[0].Address)
	}
	if refs[1].Address != "azurerm_subnet.app[1]" {
		t.Errorf("refs[1].Address = %q, want azurerm_subnet.app[1]", refs[1].Address)
	}
}
