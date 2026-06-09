package envdrift

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/terraform-state-manager/terraform-state-manager/internal/clients/cloud/azure"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/models"
)

const (
	vnetID    = "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg-platform/providers/Microsoft.Network/virtualNetworks/vnet-main"
	storageID = "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg-platform/providers/Microsoft.Storage/storageAccounts/sttfstateprod"
	vmGoneID  = "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg-platform/providers/Microsoft.Compute/virtualMachines/vm-gone"
)

// fakeDriftRepo captures the drift event the service writes.
type fakeDriftRepo struct {
	created []*models.DriftEvent
}

func (f *fakeDriftRepo) Create(_ context.Context, event *models.DriftEvent) error {
	event.ID = "evt-" + string(rune('a'+len(f.created)))
	f.created = append(f.created, event)
	return nil
}

// present builds a present ResourceState with the given properties.
func present(id string, props map[string]string) azure.ResourceState {
	return azure.ResourceState{ID: id, Existence: azure.ExistencePresent, Properties: props}
}

func TestCompare_Classifications(t *testing.T) {
	reader := azure.NewStubReader(map[string]azure.ResourceState{
		// vnet present and matching state props.
		vnetID: present(vnetID, map[string]string{azure.PropLocation: "eastus"}),
		// storage present but SKU changed out of band.
		storageID: present(storageID, map[string]string{
			azure.PropLocation: "eastus",
			azure.PropSKU:      "Standard_GRS",
		}),
		// vmGoneID intentionally absent -> StubReader default missing.
	})

	refs := []StateResourceRef{
		{Address: "azurerm_virtual_network.main", Type: "azurerm_virtual_network", ARMID: vnetID},
		{Address: "azurerm_storage_account.state", Type: "azurerm_storage_account", ARMID: storageID},
		{Address: "azurerm_linux_virtual_machine.app", Type: "azurerm_linux_virtual_machine", ARMID: vmGoneID},
	}

	stateProps := map[string]map[string]string{
		vnetID:    {azure.PropLocation: "eastus"},
		storageID: {azure.PropLocation: "eastus", azure.PropSKU: "Standard_LRS"},
	}

	changes, err := Compare(context.Background(), reader, refs, stateProps)
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}

	if changes.PresentCount != 1 {
		t.Errorf("PresentCount = %d, want 1", changes.PresentCount)
	}
	if changes.MissingCount != 1 {
		t.Errorf("MissingCount = %d, want 1", changes.MissingCount)
	}
	if changes.ChangedCount != 1 {
		t.Errorf("ChangedCount = %d, want 1", changes.ChangedCount)
	}
	if !changes.HasDrift() {
		t.Error("HasDrift() = false, want true")
	}

	if len(changes.Removed) != 1 || changes.Removed[0] != "azurerm_linux_virtual_machine.app" {
		t.Errorf("Removed = %v, want [azurerm_linux_virtual_machine.app]", changes.Removed)
	}
	if len(changes.Modified) != 1 || changes.Modified[0] != "azurerm_storage_account.state" {
		t.Errorf("Modified = %v, want [azurerm_storage_account.state]", changes.Modified)
	}

	// Verify the changed finding records exactly the sku property.
	var changedFinding *ResourceFinding
	for i := range changes.Findings {
		if changes.Findings[i].Classification == ClassChanged {
			changedFinding = &changes.Findings[i]
		}
	}
	if changedFinding == nil {
		t.Fatal("no changed finding recorded")
	}
	if len(changedFinding.ChangedProps) != 1 || changedFinding.ChangedProps[0] != azure.PropSKU {
		t.Errorf("ChangedProps = %v, want [sku]", changedFinding.ChangedProps)
	}
}

func TestCompare_UnknownDoesNotCountAsDrift(t *testing.T) {
	reader := &azure.StubReader{Responses: map[string]azure.ResourceState{}, DefaultMissing: false}
	refs := []StateResourceRef{
		{Address: "azurerm_virtual_network.main", Type: "azurerm_virtual_network", ARMID: vnetID},
	}

	changes, err := Compare(context.Background(), reader, refs, nil)
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if changes.UnknownCount != 1 {
		t.Errorf("UnknownCount = %d, want 1", changes.UnknownCount)
	}
	if changes.HasDrift() {
		t.Error("HasDrift() = true, want false for unknown-only result")
	}
}

func TestCompare_ExistenceOnlyWhenNoStateProps(t *testing.T) {
	reader := azure.NewStubReader(map[string]azure.ResourceState{
		vnetID: present(vnetID, map[string]string{azure.PropLocation: "westus"}),
	})
	refs := []StateResourceRef{
		{Address: "azurerm_virtual_network.main", Type: "azurerm_virtual_network", ARMID: vnetID},
	}

	// No state props -> existence-only; differing live props must NOT be drift.
	changes, err := Compare(context.Background(), reader, refs, nil)
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if changes.ChangedCount != 0 {
		t.Errorf("ChangedCount = %d, want 0 for existence-only comparison", changes.ChangedCount)
	}
	if changes.PresentCount != 1 {
		t.Errorf("PresentCount = %d, want 1", changes.PresentCount)
	}
}

func TestDetectForState_WritesDriftEvent(t *testing.T) {
	state := loadState(t)
	reader := azure.NewStubReader(map[string]azure.ResourceState{
		vnetID:    present(vnetID, map[string]string{azure.PropLocation: "eastus"}),
		storageID: present(storageID, map[string]string{azure.PropLocation: "eastus"}),
		// vm-gone absent -> missing -> drift.
	})
	repo := &fakeDriftRepo{}
	svc := &Service{reader: reader, driftRepo: repo}

	result, err := svc.DetectForState(context.Background(), "org-1", "prod-vnet", state, nil)
	if err != nil {
		t.Fatalf("DetectForState: %v", err)
	}

	if len(repo.created) != 1 {
		t.Fatalf("expected 1 drift event written, got %d", len(repo.created))
	}
	event := repo.created[0]
	if event.DriftSource != models.DriftSourceEnvironment {
		t.Errorf("DriftSource = %q, want environment", event.DriftSource)
	}
	if event.OrganizationID != "org-1" {
		t.Errorf("OrganizationID = %q, want org-1", event.OrganizationID)
	}
	if event.WorkspaceName != "prod-vnet" {
		t.Errorf("WorkspaceName = %q, want prod-vnet", event.WorkspaceName)
	}
	// One missing resource -> critical severity.
	if event.Severity != models.DriftSeverityCritical {
		t.Errorf("Severity = %q, want critical", event.Severity)
	}
	if result.DriftEventID == "" {
		t.Error("Result.DriftEventID is empty")
	}

	// The changes JSONB round-trips into the env-drift shape.
	var changes DriftChanges
	if err := json.Unmarshal(event.Changes, &changes); err != nil {
		t.Fatalf("unmarshalling changes JSONB: %v", err)
	}
	if changes.MissingCount != 1 {
		t.Errorf("changes.MissingCount = %d, want 1", changes.MissingCount)
	}
}

func TestDetectForState_NoDriftNoEvent(t *testing.T) {
	state := loadState(t)
	reader := azure.NewStubReader(map[string]azure.ResourceState{
		vnetID:    present(vnetID, nil),
		storageID: present(storageID, nil),
		vmGoneID:  present(vmGoneID, nil),
	})
	repo := &fakeDriftRepo{}
	svc := &Service{reader: reader, driftRepo: repo}

	result, err := svc.DetectForState(context.Background(), "org-1", "prod-vnet", state, nil)
	if err != nil {
		t.Fatalf("DetectForState: %v", err)
	}
	if len(repo.created) != 0 {
		t.Errorf("expected no drift event when all present, got %d", len(repo.created))
	}
	if result.DriftEventID != "" {
		t.Errorf("DriftEventID = %q, want empty", result.DriftEventID)
	}
	if result.Severity != models.DriftSeverityInfo {
		t.Errorf("Severity = %q, want info", result.Severity)
	}
}
