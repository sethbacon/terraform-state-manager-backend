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

// v3State is a Terraform 0.11.x (format v3) state: resources live under
// modules[].resources keyed by address, with primary/deposed instances instead of
// v4's resources[].instances[]. It exercises count expansion (node.0 / node.1), a
// child module (vpc), a null_resource (excluded from RUM), and a data source
// (excluded from managed/RUM).
const v3State = `{
  "version": 3,
  "terraform_version": "0.11.11",
  "serial": 5,
  "lineage": "v3-lineage",
  "modules": [
    {
      "path": ["root"],
      "outputs": {},
      "resources": {
        "aws_instance.web": {
          "type": "aws_instance",
          "primary": {"id": "i-web"},
          "deposed": [],
          "provider": "provider.aws"
        },
        "aws_instance.node.0": {
          "type": "aws_instance",
          "primary": {"id": "i-node0"},
          "provider": "provider.aws"
        },
        "aws_instance.node.1": {
          "type": "aws_instance",
          "primary": {"id": "i-node1"},
          "provider": "provider.aws"
        },
        "null_resource.trigger": {
          "type": "null_resource",
          "primary": {"id": "123"},
          "provider": "provider.null"
        },
        "data.aws_ami.ubuntu": {
          "type": "aws_ami",
          "primary": {"id": "ami-1"},
          "provider": "provider.aws"
        }
      }
    },
    {
      "path": ["root", "vpc"],
      "resources": {
        "aws_subnet.a": {
          "type": "aws_subnet",
          "primary": {"id": "subnet-a"},
          "provider": "provider.aws"
        }
      }
    }
  ]
}`

func TestAnalyzeV3Counts(t *testing.T) {
	a, err := Analyze([]byte(v3State))
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if a.FormatVersion != 3 {
		t.Errorf("format_version = %d, want 3", a.FormatVersion)
	}
	if a.TerraformVersion != "0.11.11" {
		t.Errorf("terraform_version = %q", a.TerraformVersion)
	}
	// managed: web + node.0 + node.1 + null_resource + subnet = 5
	if a.ManagedResources != 5 {
		t.Errorf("managed = %d, want 5", a.ManagedResources)
	}
	if a.DataSources != 1 {
		t.Errorf("data sources = %d, want 1", a.DataSources)
	}
	if a.TotalResources != 6 {
		t.Errorf("total = %d, want 6", a.TotalResources)
	}
	if a.NullResources != 1 {
		t.Errorf("null/terraform_data = %d, want 1", a.NullResources)
	}
	// RUM = managed(5) - null(1) = 4
	if a.RUM != 4 {
		t.Errorf("RUM = %d, want 4", a.RUM)
	}

	types := map[string]int{}
	for _, c := range a.ResourceTypes {
		types[c.Key] = c.Count
	}
	if types["aws_instance"] != 3 {
		t.Errorf("aws_instance = %d, want 3", types["aws_instance"])
	}
	if types["aws_subnet"] != 1 {
		t.Errorf("aws_subnet = %d, want 1", types["aws_subnet"])
	}

	providers := map[string]int{}
	for _, c := range a.Providers {
		providers[c.Key] = c.Count
	}
	// v3 provider refs ("provider.aws") normalize to the bare name.
	if providers["aws"] != 4 {
		t.Errorf("aws provider = %d, want 4", providers["aws"])
	}
	if providers["null"] != 1 {
		t.Errorf("null provider = %d, want 1", providers["null"])
	}

	modules := map[string]int{}
	for _, c := range a.Modules {
		modules[c.Key] = c.Count
	}
	if modules["root"] != 4 {
		t.Errorf("root module = %d, want 4", modules["root"])
	}
	if modules["module.vpc"] != 1 {
		t.Errorf("module.vpc = %d, want 1", modules["module.vpc"])
	}
}

// TestAnalyzeV3Deposed verifies a v3 resource's deposed instances are counted
// alongside its primary.
func TestAnalyzeV3Deposed(t *testing.T) {
	const s = `{
  "version": 3,
  "terraform_version": "0.11.11",
  "lineage": "dep",
  "modules": [
    {
      "path": ["root"],
      "resources": {
        "aws_instance.web": {
          "type": "aws_instance",
          "primary": {"id": "new"},
          "deposed": [{"id": "old"}],
          "provider": "provider.aws"
        }
      }
    }
  ]
}`
	a, err := Analyze([]byte(s))
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	// primary + 1 deposed = 2 managed instances
	if a.ManagedResources != 2 {
		t.Errorf("managed = %d, want 2 (primary + deposed)", a.ManagedResources)
	}
	if a.RUM != 2 {
		t.Errorf("RUM = %d, want 2", a.RUM)
	}
}

// TestListResourcesV3 confirms the resource-list view also understands v3 states.
func TestListResourcesV3(t *testing.T) {
	rs, err := ListResources([]byte(v3State))
	if err != nil {
		t.Fatalf("ListResources: %v", err)
	}
	// 6 resource entries across both modules (each v3 key is one entry).
	if len(rs) != 6 {
		t.Fatalf("len(resources) = %d, want 6", len(rs))
	}
	var subnet *ResourceSummary
	for i := range rs {
		if rs[i].Type == "aws_subnet" {
			subnet = &rs[i]
		}
	}
	if subnet == nil {
		t.Fatal("aws_subnet not found in v3 resource list")
	}
	if subnet.Module != "module.vpc" {
		t.Errorf("subnet module = %q, want module.vpc", subnet.Module)
	}
	if subnet.Provider != "aws" {
		t.Errorf("subnet provider = %q, want aws", subnet.Provider)
	}
}

// TestAnalyzeV3TypeFallback covers v3 entries that omit the "type" field (the type
// is then derived from the resource key) and an entry with a null primary and no
// deposed instances (which contributes zero instances and is skipped).
func TestAnalyzeV3TypeFallback(t *testing.T) {
	const s = `{
  "version": 3,
  "terraform_version": "0.11.8",
  "lineage": "fallback",
  "modules": [
    {
      "path": ["root"],
      "resources": {
        "aws_s3_bucket.data": {
          "primary": {"id": "b1"},
          "provider": "provider.aws"
        },
        "data.aws_caller_identity.current": {
          "primary": {"id": "acct"},
          "provider": "provider.aws"
        },
        "null_resource.gone": {
          "type": "null_resource",
          "primary": null,
          "deposed": []
        }
      }
    }
  ]
}`
	a, err := Analyze([]byte(s))
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	// Only the s3 bucket is a counted managed instance; the data source is excluded
	// and the null-primary null_resource contributes zero instances.
	if a.ManagedResources != 1 {
		t.Errorf("managed = %d, want 1", a.ManagedResources)
	}
	if a.DataSources != 1 {
		t.Errorf("data sources = %d, want 1", a.DataSources)
	}
	if a.NullResources != 0 {
		t.Errorf("null = %d, want 0 (null-primary entry skipped)", a.NullResources)
	}
	types := map[string]int{}
	for _, c := range a.ResourceTypes {
		types[c.Key] = c.Count
	}
	// Type derived from the key "aws_s3_bucket.data" despite the missing "type".
	if types["aws_s3_bucket"] != 1 {
		t.Errorf("aws_s3_bucket = %d, want 1 (type derived from key)", types["aws_s3_bucket"])
	}
}
