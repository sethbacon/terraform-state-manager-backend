package analyzer

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseState_ValidV4WithResources(t *testing.T) {
	state := []byte(`{
		"version": 4,
		"terraform_version": "1.5.0",
		"serial": 7,
		"lineage": "abc-123",
		"resources": [
			{
				"mode": "managed",
				"type": "aws_instance",
				"name": "web",
				"provider": "provider[\"registry.terraform.io/hashicorp/aws\"]",
				"instances": [
					{"attributes": {"id": "i-12345"}},
					{"attributes": {"id": "i-67890"}}
				]
			},
			{
				"mode": "managed",
				"type": "aws_s3_bucket",
				"name": "my_bucket",
				"provider": "provider[\"registry.terraform.io/hashicorp/aws\"]",
				"instances": [
					{"attributes": {"bucket": "my-bucket"}}
				]
			}
		]
	}`)

	counts, err := ParseState(state)
	require.NoError(t, err)
	require.NotNil(t, counts)

	assert.Equal(t, 3, counts.Total)
	assert.Equal(t, 3, counts.Managed)
	assert.Equal(t, 0, counts.DataSources)
	assert.Equal(t, 3, counts.RUM)
	assert.Equal(t, "1.5.0", counts.TerraformVersion)
	assert.Equal(t, 7, counts.Serial)
	assert.Equal(t, "abc-123", counts.Lineage)
	assert.Equal(t, 2, counts.ByType["aws_instance"])
	assert.Equal(t, 1, counts.ByType["aws_s3_bucket"])
}

func TestParseState_DataSources(t *testing.T) {
	state := []byte(`{
		"version": 4,
		"terraform_version": "1.4.6",
		"serial": 2,
		"lineage": "xyz-456",
		"resources": [
			{
				"mode": "managed",
				"type": "aws_instance",
				"name": "app",
				"provider": "provider[\"registry.terraform.io/hashicorp/aws\"]",
				"instances": [
					{"attributes": {"id": "i-aaa"}}
				]
			},
			{
				"mode": "data",
				"type": "aws_ami",
				"name": "latest",
				"provider": "provider[\"registry.terraform.io/hashicorp/aws\"]",
				"instances": [
					{"attributes": {"id": "ami-000"}},
					{"attributes": {"id": "ami-111"}}
				]
			}
		]
	}`)

	counts, err := ParseState(state)
	require.NoError(t, err)
	require.NotNil(t, counts)

	assert.Equal(t, 3, counts.Total)
	assert.Equal(t, 1, counts.Managed)
	assert.Equal(t, 2, counts.DataSources)
	assert.Equal(t, 1, counts.RUM)
}

func TestParseState_ExcludedResourceTypes(t *testing.T) {
	state := []byte(`{
		"version": 4,
		"terraform_version": "1.5.0",
		"serial": 1,
		"lineage": "test-lineage",
		"resources": [
			{
				"mode": "managed",
				"type": "aws_instance",
				"name": "web",
				"provider": "provider[\"registry.terraform.io/hashicorp/aws\"]",
				"instances": [{"attributes": {}}]
			},
			{
				"mode": "managed",
				"type": "null_resource",
				"name": "trigger",
				"provider": "provider[\"registry.terraform.io/hashicorp/null\"]",
				"instances": [{"attributes": {}}]
			},
			{
				"mode": "managed",
				"type": "terraform_data",
				"name": "init",
				"provider": "provider[\"terraform.io/builtin/terraform\"]",
				"instances": [{"attributes": {}}]
			}
		]
	}`)

	counts, err := ParseState(state)
	require.NoError(t, err)
	require.NotNil(t, counts)

	assert.Equal(t, 3, counts.Total)
	assert.Equal(t, 3, counts.Managed)
	assert.Equal(t, 2, counts.ExcludedNull)
	// RUM = 3 managed - 2 excluded = 1
	assert.Equal(t, 1, counts.RUM)
}

func TestParseState_EmptyState(t *testing.T) {
	state := []byte(`{
		"version": 4,
		"terraform_version": "1.5.0",
		"serial": 0,
		"lineage": "empty-lineage",
		"resources": []
	}`)

	counts, err := ParseState(state)
	require.NoError(t, err)
	require.NotNil(t, counts)

	assert.Equal(t, 0, counts.Total)
	assert.Equal(t, 0, counts.Managed)
	assert.Equal(t, 0, counts.DataSources)
	assert.Equal(t, 0, counts.RUM)
}

func TestParseState_NoResourcesKey(t *testing.T) {
	// State JSON with no "resources" or "modules" key at all
	state := []byte(`{
		"version": 4,
		"terraform_version": "1.5.0",
		"serial": 1,
		"lineage": "no-resources"
	}`)

	counts, err := ParseState(state)
	require.NoError(t, err)
	require.NotNil(t, counts)

	assert.Equal(t, 0, counts.Total)
	assert.Equal(t, 0, counts.RUM)
}

func TestParseState_MalformedJSON(t *testing.T) {
	state := []byte(`{ this is not valid json }`)

	counts, err := ParseState(state)
	assert.Error(t, err)
	assert.Nil(t, counts)
}

func TestParseState_EmptyInput(t *testing.T) {
	counts, err := ParseState([]byte{})
	assert.Error(t, err)
	assert.Nil(t, counts)
}

func TestParseState_LegacyModulesFormat(t *testing.T) {
	state := []byte(`{
		"version": 1,
		"terraform_version": "0.11.0",
		"serial": 3,
		"lineage": "legacy-lineage",
		"modules": [
			{
				"path": ["root"],
				"resources": {
					"aws_instance.web": {"type": "aws_instance"},
					"aws_s3_bucket.data": {"type": "aws_s3_bucket"}
				}
			}
		]
	}`)

	counts, err := ParseState(state)
	require.NoError(t, err)
	require.NotNil(t, counts)

	assert.Equal(t, 2, counts.Total)
	assert.Equal(t, 2, counts.Managed)
	assert.Equal(t, 2, counts.RUM)
	assert.Equal(t, 1, counts.ByType["aws_instance"])
	assert.Equal(t, 1, counts.ByType["aws_s3_bucket"])
}

func TestParseState_LegacyModulesWithExcluded(t *testing.T) {
	state := []byte(`{
		"version": 1,
		"serial": 1,
		"lineage": "legacy-exc",
		"modules": [
			{
				"path": ["root"],
				"resources": {
					"aws_instance.web": {"type": "aws_instance"},
					"null_resource.wait": {"type": "null_resource"}
				}
			}
		]
	}`)

	counts, err := ParseState(state)
	require.NoError(t, err)
	require.NotNil(t, counts)

	assert.Equal(t, 2, counts.Total)
	assert.Equal(t, 2, counts.Managed)
	assert.Equal(t, 1, counts.ExcludedNull)
	// RUM = 2 - 1 = 1
	assert.Equal(t, 1, counts.RUM)
}

func TestParseState_MultipleInstances(t *testing.T) {
	state := []byte(`{
		"version": 4,
		"serial": 10,
		"resources": [
			{
				"mode": "managed",
				"type": "aws_instance",
				"name": "cluster",
				"provider": "provider[\"registry.terraform.io/hashicorp/aws\"]",
				"instances": [
					{"attributes": {"id": "i-1"}},
					{"attributes": {"id": "i-2"}},
					{"attributes": {"id": "i-3"}}
				]
			}
		]
	}`)

	counts, err := ParseState(state)
	require.NoError(t, err)
	require.NotNil(t, counts)

	assert.Equal(t, 3, counts.Total)
	assert.Equal(t, 3, counts.Managed)
	assert.Equal(t, 3, counts.ByType["aws_instance"])
}

func TestParseState_LegacyModulesSubModulePath(t *testing.T) {
	// Covers the len(mod.Path) > 1 branch in parseLegacyModules
	state := []byte(`{
		"version": 1,
		"serial": 4,
		"lineage": "submodule-test",
		"modules": [
			{
				"path": ["root"],
				"resources": {
					"aws_instance.root_web": {"type": "aws_instance"}
				}
			},
			{
				"path": ["root", "network"],
				"resources": {
					"aws_vpc.main": {"type": "aws_vpc"},
					"aws_subnet.private": {"type": "aws_subnet"}
				}
			},
			{
				"path": ["root", "compute", "worker"],
				"resources": {
					"aws_instance.worker": {"type": "aws_instance"}
				}
			}
		]
	}`)

	counts, err := ParseState(state)
	require.NoError(t, err)
	require.NotNil(t, counts)

	assert.Equal(t, 4, counts.Total)
	assert.Equal(t, 4, counts.Managed)
	// Sub-module resources should be recorded under their module path
	assert.Equal(t, 1, counts.ByModule["root"])
	assert.Equal(t, 2, counts.ByModule["network"])
	assert.Equal(t, 1, counts.ByModule["compute.worker"])
}

func TestParseState_ResourcesWithExplicitModule(t *testing.T) {
	// Exercises the resource.Module != "" branch in CountResources
	state := []byte(`{
		"version": 4,
		"serial": 11,
		"resources": [
			{
				"mode": "managed",
				"type": "aws_instance",
				"name": "web",
				"module": "module.compute",
				"provider": "provider[\"registry.terraform.io/hashicorp/aws\"]",
				"instances": [{"attributes": {}}]
			},
			{
				"mode": "managed",
				"type": "aws_db_instance",
				"name": "db",
				"module": "module.database",
				"provider": "provider[\"registry.terraform.io/hashicorp/aws\"]",
				"instances": [{"attributes": {}}, {"attributes": {}}]
			},
			{
				"mode": "data",
				"type": "aws_ami",
				"name": "base",
				"module": "module.compute",
				"provider": "provider[\"registry.terraform.io/hashicorp/aws\"]",
				"instances": [{"attributes": {}}]
			}
		]
	}`)

	counts, err := ParseState(state)
	require.NoError(t, err)
	require.NotNil(t, counts)

	assert.Equal(t, 4, counts.Total)
	assert.Equal(t, 3, counts.Managed)
	assert.Equal(t, 1, counts.DataSources)
	assert.Equal(t, 2, counts.ByModule["module.compute"]) // both managed (1) and data (1) instances count toward ByModule
	assert.Equal(t, 2, counts.ByModule["module.database"])
}

func TestParseState_EmptyModulesArray(t *testing.T) {
	// Covers the "modules key present but empty" branch in ParseState
	state := []byte(`{
		"version": 1,
		"serial": 1,
		"lineage": "empty-modules",
		"modules": []
	}`)

	counts, err := ParseState(state)
	require.NoError(t, err)
	require.NotNil(t, counts)

	assert.Equal(t, 0, counts.Total)
	assert.Equal(t, 0, counts.RUM)
}

func TestParseState_LegacyModulesEmptyPathSegment(t *testing.T) {
	// Covers the `if moduleName == "" { moduleName = "root" }` fallback in parseLegacyModules
	// This is triggered when path has >1 segments but all non-root segments are empty strings.
	state := []byte(`{
		"version": 1,
		"serial": 5,
		"modules": [
			{
				"path": ["root", ""],
				"resources": {
					"aws_instance.test": {"type": "aws_instance"}
				}
			}
		]
	}`)

	counts, err := ParseState(state)
	require.NoError(t, err)
	require.NotNil(t, counts)
	assert.Equal(t, 1, counts.Total)
	// The empty segment path collapses to "root"
	assert.Equal(t, 1, counts.ByModule["root"])
}
