package driftingest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func loadPlan(t *testing.T, name string) *Plan {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	var plan Plan
	if err := json.Unmarshal(data, &plan); err != nil {
		t.Fatalf("unmarshal fixture %s: %v", name, err)
	}
	return &plan
}

func TestSummarizePlan_WithChanges(t *testing.T) {
	plan := loadPlan(t, "plan_with_changes.json")
	changes := SummarizePlan(plan)

	if !changes.HasChanges() {
		t.Fatal("expected HasChanges() to be true")
	}
	if got, want := len(changes.Added), 1; got != want {
		t.Errorf("added = %d, want %d (%v)", got, want, changes.Added)
	}
	if got, want := len(changes.Removed), 1; got != want {
		t.Errorf("removed = %d, want %d (%v)", got, want, changes.Removed)
	}
	if len(changes.Modified) != 0 {
		t.Errorf("modified = %v, want none", changes.Modified)
	}
	// One create (+1), one delete (-1), one no-op (0) => net zero.
	if changes.ResourceDelta != 0 {
		t.Errorf("resource_delta = %d, want 0", changes.ResourceDelta)
	}
	if changes.Added[0] != "null_resource.example" {
		t.Errorf("added[0] = %q, want null_resource.example", changes.Added[0])
	}
	if changes.Removed[0] != "null_resource.obsolete" {
		t.Errorf("removed[0] = %q, want null_resource.obsolete", changes.Removed[0])
	}
}

func TestSummarizePlan_NoChanges(t *testing.T) {
	plan := loadPlan(t, "plan_no_changes.json")
	changes := SummarizePlan(plan)

	if changes.HasChanges() {
		t.Errorf("expected no changes, got %+v", changes)
	}
	if changes.ResourceDelta != 0 {
		t.Errorf("resource_delta = %d, want 0", changes.ResourceDelta)
	}
}

func TestSummarizePlan_Replacement(t *testing.T) {
	plan := &Plan{
		ResourceChanges: []ResourceChange{
			{Address: "aws_instance.web", Change: Change{Actions: []string{"delete", "create"}}},
		},
	}
	changes := SummarizePlan(plan)

	if len(changes.Modified) != 1 || changes.Modified[0] != "aws_instance.web" {
		t.Errorf("modified = %v, want [aws_instance.web]", changes.Modified)
	}
	if len(changes.Added) != 0 || len(changes.Removed) != 0 {
		t.Errorf("replacement should be modified-only, got added=%v removed=%v", changes.Added, changes.Removed)
	}
	// Replacement is net-zero on resource count.
	if changes.ResourceDelta != 0 {
		t.Errorf("resource_delta = %d, want 0", changes.ResourceDelta)
	}
}

func TestSummarizePlan_Update(t *testing.T) {
	plan := &Plan{
		ResourceChanges: []ResourceChange{
			{Address: "aws_s3_bucket.b", Change: Change{Actions: []string{"update"}}},
		},
	}
	changes := SummarizePlan(plan)

	if len(changes.Modified) != 1 {
		t.Errorf("modified = %v, want one", changes.Modified)
	}
	if changes.ResourceDelta != 0 {
		t.Errorf("resource_delta = %d, want 0 for in-place update", changes.ResourceDelta)
	}
}

func TestSummarizePlan_Nil(t *testing.T) {
	changes := SummarizePlan(nil)
	if changes.HasChanges() {
		t.Error("nil plan should yield no changes")
	}
}
