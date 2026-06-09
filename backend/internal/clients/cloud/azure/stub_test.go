package azure

import (
	"context"
	"testing"
)

func TestStubReader_PresentFromFixture(t *testing.T) {
	present, err := PresentFromARMJSON(vnetID, loadFixture(t, "virtual_network.json"))
	if err != nil {
		t.Fatalf("PresentFromARMJSON: %v", err)
	}
	if present.Existence != ExistencePresent {
		t.Fatalf("Existence = %q, want present", present.Existence)
	}
	if present.Properties[PropLocation] != "eastus" {
		t.Errorf("location = %q, want eastus", present.Properties[PropLocation])
	}

	reader := NewStubReader(map[string]ResourceState{vnetID: present})

	got, err := reader.ReadResource(context.Background(), vnetID)
	if err != nil {
		t.Fatalf("ReadResource: %v", err)
	}
	if got.Existence != ExistencePresent {
		t.Errorf("Existence = %q, want present", got.Existence)
	}
	if got.ID != vnetID {
		t.Errorf("ID = %q, want %q", got.ID, vnetID)
	}
}

func TestStubReader_DefaultMissing(t *testing.T) {
	reader := NewStubReader(nil)
	got, err := reader.ReadResource(context.Background(), storageID)
	if err != nil {
		t.Fatalf("ReadResource: %v", err)
	}
	if got.Existence != ExistenceMissing {
		t.Errorf("Existence = %q, want missing (default)", got.Existence)
	}
}

func TestStubReader_UnknownWhenNotDefaultMissing(t *testing.T) {
	reader := &StubReader{Responses: map[string]ResourceState{}, DefaultMissing: false}
	got, err := reader.ReadResource(context.Background(), storageID)
	if err != nil {
		t.Fatalf("ReadResource: %v", err)
	}
	if got.Existence != ExistenceUnknown {
		t.Errorf("Existence = %q, want unknown", got.Existence)
	}
}

func TestStubReader_UnparseableID(t *testing.T) {
	reader := NewStubReader(nil)
	got, err := reader.ReadResource(context.Background(), "garbage")
	if err != nil {
		t.Fatalf("ReadResource: %v", err)
	}
	if got.Existence != ExistenceUnknown {
		t.Errorf("Existence = %q, want unknown", got.Existence)
	}
}

func TestPresentFromARMJSON_InvalidJSON(t *testing.T) {
	if _, err := PresentFromARMJSON(vnetID, []byte("{not json")); err == nil {
		t.Error("expected error for invalid JSON fixture")
	}
}
