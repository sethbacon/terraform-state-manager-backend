package analyzer

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseLockFile_Fixture(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("testdata", ".terraform.lock.hcl"))
	require.NoError(t, err)

	pins, err := ParseLockFile(src)
	require.NoError(t, err)
	require.Len(t, pins, 2)

	aws, ok := pins["registry.terraform.io/hashicorp/aws"]
	require.True(t, ok)
	assert.Equal(t, "5.31.0", aws.Version)
	assert.Equal(t, ">= 5.0.0, < 6.0.0", aws.Constraints)

	random, ok := pins["registry.terraform.io/hashicorp/random"]
	require.True(t, ok)
	assert.Equal(t, "3.6.0", random.Version)
	assert.Equal(t, "~> 3.0", random.Constraints)
}

func TestParseLockFile_Empty(t *testing.T) {
	pins, err := ParseLockFile([]byte(""))
	require.NoError(t, err)
	assert.Empty(t, pins)
}

func TestParseLockFile_NoConstraints(t *testing.T) {
	src := `provider "registry.terraform.io/hashicorp/tls" {
  version = "4.0.5"
}`
	pins, err := ParseLockFile([]byte(src))
	require.NoError(t, err)
	require.Len(t, pins, 1)
	tls := pins["registry.terraform.io/hashicorp/tls"]
	assert.Equal(t, "4.0.5", tls.Version)
	assert.Equal(t, "", tls.Constraints)
}

func TestParseLockFile_InvalidHCL(t *testing.T) {
	_, err := ParseLockFile([]byte(`provider "x" { version = `))
	assert.Error(t, err)
}

func TestSortedLockPins(t *testing.T) {
	pins := map[string]ProviderLockPin{
		"registry.terraform.io/hashicorp/random": {Source: "registry.terraform.io/hashicorp/random", Version: "3.6.0"},
		"registry.terraform.io/hashicorp/aws":    {Source: "registry.terraform.io/hashicorp/aws", Version: "5.31.0"},
	}
	sorted := SortedLockPins(pins)
	require.Len(t, sorted, 2)
	assert.Equal(t, "registry.terraform.io/hashicorp/aws", sorted[0].Source)
	assert.Equal(t, "registry.terraform.io/hashicorp/random", sorted[1].Source)
}
