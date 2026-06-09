package analyzer

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// staticSource builds a fixture source for the pin-drift tests.
func staticSource() *StaticProviderVersionSource {
	return &StaticProviderVersionSource{
		Versions: map[string][]string{
			"hashicorp/aws":    {"5.30.0", "5.31.0", "5.40.0"},
			"hashicorp/random": {"3.6.0"},
		},
		Unknown: map[string]bool{
			"hashicorp/null": true,
		},
	}
}

func TestComputeProviderPinDrift_BehindAndUpToDate(t *testing.T) {
	pins := []ProviderLockPin{
		{Source: "registry.terraform.io/hashicorp/aws", Version: "5.31.0", Constraints: ">= 5.0.0, < 6.0.0"},
		{Source: "registry.terraform.io/hashicorp/random", Version: "3.6.0", Constraints: "~> 3.0"},
	}

	reports := ComputeProviderPinDrift(context.Background(), pins, staticSource())
	require.Len(t, reports, 2)

	aws := reports[0]
	assert.Equal(t, "5.31.0", aws.Pinned)
	assert.Equal(t, "5.40.0", aws.LatestAvailable)
	assert.Equal(t, ProviderPinBehind, aws.Status)
	assert.True(t, aws.SatisfiesConstraint)

	random := reports[1]
	assert.Equal(t, "3.6.0", random.LatestAvailable)
	assert.Equal(t, ProviderPinUpToDate, random.Status)
	assert.True(t, random.SatisfiesConstraint)
}

func TestComputeProviderPinDrift_UnknownProvider(t *testing.T) {
	pins := []ProviderLockPin{
		{Source: "registry.terraform.io/hashicorp/null", Version: "3.2.0", Constraints: "~> 3.0"},
	}

	reports := ComputeProviderPinDrift(context.Background(), pins, staticSource())
	require.Len(t, reports, 1)

	r := reports[0]
	assert.Equal(t, ProviderPinUnknown, r.Status)
	assert.Empty(t, r.LatestAvailable)
	// Constraint satisfaction is independent of the registry lookup.
	assert.True(t, r.SatisfiesConstraint)
}

func TestComputeProviderPinDrift_ConstraintViolated(t *testing.T) {
	// Pinned 4.9.0 does not satisfy ">= 5.0.0"; latest is still resolvable.
	pins := []ProviderLockPin{
		{Source: "registry.terraform.io/hashicorp/aws", Version: "4.9.0", Constraints: ">= 5.0.0, < 6.0.0"},
	}

	reports := ComputeProviderPinDrift(context.Background(), pins, staticSource())
	require.Len(t, reports, 1)

	r := reports[0]
	assert.False(t, r.SatisfiesConstraint)
	assert.Equal(t, ProviderPinBehind, r.Status) // 4.9.0 < 5.40.0
}

func TestComputeProviderPinDrift_NilSourceIsUnknown(t *testing.T) {
	pins := []ProviderLockPin{
		{Source: "registry.terraform.io/hashicorp/aws", Version: "5.31.0", Constraints: ">= 5.0.0"},
	}

	reports := ComputeProviderPinDrift(context.Background(), pins, nil)
	require.Len(t, reports, 1)
	assert.Equal(t, ProviderPinUnknown, reports[0].Status)
	assert.Empty(t, reports[0].LatestAvailable)
	// Constraint still evaluated without a registry source.
	assert.True(t, reports[0].SatisfiesConstraint)
}

func TestComputeProviderPinDrift_UnparseableConstraintLeavesSatisfiesFalse(t *testing.T) {
	pins := []ProviderLockPin{
		{Source: "registry.terraform.io/hashicorp/aws", Version: "5.31.0", Constraints: "latest"},
	}

	reports := ComputeProviderPinDrift(context.Background(), pins, staticSource())
	require.Len(t, reports, 1)
	assert.False(t, reports[0].SatisfiesConstraint)
	assert.Equal(t, ProviderPinBehind, reports[0].Status)
}

func TestComputeProviderPinDrift_NoConstraint(t *testing.T) {
	pins := []ProviderLockPin{
		{Source: "registry.terraform.io/hashicorp/random", Version: "3.6.0"},
	}

	reports := ComputeProviderPinDrift(context.Background(), pins, staticSource())
	require.Len(t, reports, 1)
	assert.False(t, reports[0].SatisfiesConstraint) // no constraint recorded
	assert.Equal(t, ProviderPinUpToDate, reports[0].Status)
}

func TestComputeProviderPinDrift_Empty(t *testing.T) {
	reports := ComputeProviderPinDrift(context.Background(), nil, staticSource())
	assert.Empty(t, reports)
}
