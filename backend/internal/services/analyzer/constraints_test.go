package analyzer

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExtractTerraformConstraints_Fixture(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("testdata", "versions.tf"))
	require.NoError(t, err)

	tc, err := ExtractTerraformConstraints(src, "versions.tf")
	require.NoError(t, err)
	require.NotNil(t, tc)

	assert.Equal(t, ">= 1.5.0, < 2.0.0", tc.RequiredVersion)

	// Only modules that declare a version are recorded; the local-path module
	// without a version constraint is skipped.
	require.Len(t, tc.ModuleConstraints, 2)

	byName := map[string]ModuleConstraint{}
	for _, mc := range tc.ModuleConstraints {
		byName[mc.Name] = mc
	}
	require.Contains(t, byName, "network")
	assert.Equal(t, "terraform-aws-modules/vpc/aws", byName["network"].Source)
	assert.Equal(t, "~> 5.0", byName["network"].Version)

	require.Contains(t, byName, "labels")
	assert.Equal(t, "0.25.0", byName["labels"].Version)

	assert.NotContains(t, byName, "local_helper")
}

func TestExtractTerraformConstraints_Inline(t *testing.T) {
	tests := []struct {
		name        string
		src         string
		wantVersion string
		wantModules int
	}{
		{
			name:        "required_version only",
			src:         `terraform { required_version = "~> 1.6" }`,
			wantVersion: "~> 1.6",
			wantModules: 0,
		},
		{
			name:        "no terraform block",
			src:         `resource "null_resource" "x" {}`,
			wantVersion: "",
			wantModules: 0,
		},
		{
			name: "terraform block without required_version",
			src: `terraform {
  backend "s3" {}
}`,
			wantVersion: "",
			wantModules: 0,
		},
		{
			name: "module with version",
			src: `module "m" {
  source  = "org/mod/aws"
  version = ">= 1.0.0"
}`,
			wantVersion: "",
			wantModules: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tc, err := ExtractTerraformConstraints([]byte(tt.src), "config.tf")
			require.NoError(t, err)
			assert.Equal(t, tt.wantVersion, tc.RequiredVersion)
			assert.Len(t, tc.ModuleConstraints, tt.wantModules)
		})
	}
}

func TestExtractTerraformConstraints_InvalidHCL(t *testing.T) {
	_, err := ExtractTerraformConstraints([]byte(`terraform { required_version = `), "bad.tf")
	assert.Error(t, err)
}

func TestExtractTerraformConstraints_NonLiteralVersionIgnored(t *testing.T) {
	// A required_version referencing a variable is not a literal string and is
	// silently skipped (left empty) rather than erroring.
	src := `terraform { required_version = var.tf_version }`
	tc, err := ExtractTerraformConstraints([]byte(src), "config.tf")
	require.NoError(t, err)
	assert.Equal(t, "", tc.RequiredVersion)
}
