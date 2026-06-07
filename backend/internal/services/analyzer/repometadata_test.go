package analyzer

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRepoMetadata_IsEmpty(t *testing.T) {
	var nilMeta *RepoMetadata
	assert.True(t, nilMeta.IsEmpty())
	assert.True(t, (&RepoMetadata{}).IsEmpty())
	assert.False(t, (&RepoMetadata{RequiredVersionSpec: ">= 1.0.0"}).IsEmpty())
	assert.False(t, (&RepoMetadata{LockFile: "provider \"x\" {}"}).IsEmpty())
}

func TestAnalyzeRepoMetadata_FromRawFiles(t *testing.T) {
	lock, err := os.ReadFile(filepath.Join("testdata", ".terraform.lock.hcl"))
	require.NoError(t, err)
	cfg, err := os.ReadFile(filepath.Join("testdata", "versions.tf"))
	require.NoError(t, err)

	meta := &RepoMetadata{
		ConfigHCL: string(cfg),
		LockFile:  string(lock),
	}

	res, err := AnalyzeRepoMetadata(meta)
	require.NoError(t, err)
	require.NotNil(t, res)

	assert.Equal(t, ">= 1.5.0, < 2.0.0", res.RequiredVersionSpec)
	require.Len(t, res.ProviderLockPins, 2)
	assert.Equal(t, "registry.terraform.io/hashicorp/aws", res.ProviderLockPins[0].Source)
	require.Len(t, res.ModuleConstraints, 2)
}

func TestAnalyzeRepoMetadata_ExplicitWins(t *testing.T) {
	meta := &RepoMetadata{
		RequiredVersionSpec: ">= 1.7.0",
		ConfigHCL:           `terraform { required_version = ">= 1.5.0" }`,
		ProviderPins: []ProviderLockPin{
			{Source: "registry.terraform.io/hashicorp/aws", Version: "5.0.0"},
		},
		ModuleConstraints: []ModuleConstraint{
			{Name: "explicit", Version: "1.0.0"},
		},
	}

	res, err := AnalyzeRepoMetadata(meta)
	require.NoError(t, err)
	// Explicit required_version overrides the one parseable from ConfigHCL.
	assert.Equal(t, ">= 1.7.0", res.RequiredVersionSpec)
	require.Len(t, res.ProviderLockPins, 1)
	assert.Equal(t, "5.0.0", res.ProviderLockPins[0].Version)
	require.Len(t, res.ModuleConstraints, 1)
	assert.Equal(t, "explicit", res.ModuleConstraints[0].Name)
}

func TestAnalyzeRepoMetadata_Nil(t *testing.T) {
	res, err := AnalyzeRepoMetadata(nil)
	require.NoError(t, err)
	assert.NotNil(t, res)
	assert.Empty(t, res.RequiredVersionSpec)
	assert.Empty(t, res.ProviderLockPins)
}

func TestAnalyzeRepoMetadata_MalformedLockFileSoftFails(t *testing.T) {
	meta := &RepoMetadata{
		RequiredVersionSpec: ">= 1.5.0",
		LockFile:            `provider "x" { version = `,
	}
	res, err := AnalyzeRepoMetadata(meta)
	// Error is surfaced (soft warning) but parseable fields still populate.
	assert.Error(t, err)
	assert.Equal(t, ">= 1.5.0", res.RequiredVersionSpec)
	assert.Empty(t, res.ProviderLockPins)
}
