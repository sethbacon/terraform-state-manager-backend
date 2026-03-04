package analyzer

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCalculateRUM_Normal(t *testing.T) {
	result := CalculateRUM(10, 2)
	assert.Equal(t, 8, result)
}

func TestCalculateRUM_ZeroExcluded(t *testing.T) {
	result := CalculateRUM(5, 0)
	assert.Equal(t, 5, result)
}

func TestCalculateRUM_AllExcluded(t *testing.T) {
	result := CalculateRUM(3, 3)
	assert.Equal(t, 0, result)
}

func TestCalculateRUM_ExcludedExceedsManaged_ReturnsZero(t *testing.T) {
	// Should never happen in practice but the function should not return negative
	result := CalculateRUM(2, 5)
	assert.Equal(t, 0, result)
}

func TestCalculateRUM_BothZero(t *testing.T) {
	result := CalculateRUM(0, 0)
	assert.Equal(t, 0, result)
}

func TestCalculateRUM_LargeNumbers(t *testing.T) {
	result := CalculateRUM(1000, 250)
	assert.Equal(t, 750, result)
}

func TestExcludedResourceTypes_ContainsExpectedTypes(t *testing.T) {
	assert.True(t, ExcludedResourceTypes["null_resource"], "null_resource should be excluded")
	assert.True(t, ExcludedResourceTypes["terraform_data"], "terraform_data should be excluded")
	assert.False(t, ExcludedResourceTypes["aws_instance"], "aws_instance should not be excluded")
}
