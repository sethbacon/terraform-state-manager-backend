package analyzer

import "testing"

// sampleState exercises count expansion (2 aws_instance instances), a second
// provider (azurerm), a null_resource and a terraform_data (excluded from RUM),
// and a data source (excluded from managed/RUM).
const sampleState = `{
  "version": 4,
  "terraform_version": "1.7.5",
  "serial": 42,
  "lineage": "abc-123",
  "resources": [
    {
      "mode": "managed", "type": "aws_instance", "name": "web",
      "provider": "provider[\"registry.terraform.io/hashicorp/aws\"]",
      "instances": [ {"x": 1}, {"x": 2} ]
    },
    {
      "mode": "managed", "type": "azurerm_resource_group", "name": "rg",
      "provider": "provider[\"registry.terraform.io/hashicorp/azurerm\"]",
      "instances": [ {"x": 1} ]
    },
    {
      "mode": "managed", "type": "null_resource", "name": "trigger",
      "provider": "provider[\"registry.terraform.io/hashicorp/null\"]",
      "instances": [ {"x": 1} ]
    },
    {
      "mode": "managed", "type": "terraform_data", "name": "store",
      "provider": "provider[\"terraform.io/builtin/terraform\"]",
      "instances": [ {"x": 1} ]
    },
    {
      "mode": "data", "type": "aws_ami", "name": "ubuntu",
      "provider": "provider[\"registry.terraform.io/hashicorp/aws\"]",
      "instances": [ {"x": 1} ]
    }
  ]
}`

func TestAnalyzeCounts(t *testing.T) {
	a, err := Analyze([]byte(sampleState))
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}

	if a.TerraformVersion != "1.7.5" {
		t.Errorf("terraform_version = %q", a.TerraformVersion)
	}
	if a.Serial != 42 {
		t.Errorf("serial = %d", a.Serial)
	}
	// managed instances: 2 + 1 + 1 + 1 = 5
	if a.ManagedResources != 5 {
		t.Errorf("managed = %d, want 5", a.ManagedResources)
	}
	if a.DataSources != 1 {
		t.Errorf("data sources = %d, want 1", a.DataSources)
	}
	if a.TotalResources != 6 {
		t.Errorf("total = %d, want 6", a.TotalResources)
	}
	// null_resource + terraform_data = 2 excluded
	if a.NullResources != 2 {
		t.Errorf("null/terraform_data = %d, want 2", a.NullResources)
	}
	// RUM = managed(5) - excluded(2) = 3
	if a.RUM != 3 {
		t.Errorf("RUM = %d, want 3", a.RUM)
	}
}

func TestAnalyzeProviderNormalization(t *testing.T) {
	a, err := Analyze([]byte(sampleState))
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	got := map[string]int{}
	for _, c := range a.Providers {
		got[c.Key] = c.Count
	}
	if got["hashicorp/aws"] != 2 {
		t.Errorf("hashicorp/aws = %d, want 2 (data source excluded)", got["hashicorp/aws"])
	}
	if got["hashicorp/azurerm"] != 1 {
		t.Errorf("hashicorp/azurerm = %d, want 1", got["hashicorp/azurerm"])
	}
	// Top resource type should be aws_instance (2 instances).
	if len(a.ResourceTypes) == 0 || a.ResourceTypes[0].Key != "aws_instance" || a.ResourceTypes[0].Count != 2 {
		t.Errorf("top resource type = %+v, want aws_instance:2", a.ResourceTypes)
	}
}

func TestAnalyzeRejectsNonState(t *testing.T) {
	if _, err := Analyze([]byte(`{"hello":"world"}`)); err == nil {
		t.Error("expected error for non-state JSON")
	}
	if _, err := Analyze([]byte(`not json`)); err == nil {
		t.Error("expected error for invalid JSON")
	}
}
