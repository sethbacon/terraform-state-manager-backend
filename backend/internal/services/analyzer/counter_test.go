package analyzer

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/terraform-state-manager/terraform-state-manager/internal/clients/hcp"
)

func TestCountResources_EmptyMode(t *testing.T) {
	// Exercises the `if mode == "" { mode = "managed" }` branch in CountResources
	resources := []hcp.StateResource{
		{
			// Mode is empty — should be treated as "managed"
			Type:      "aws_instance",
			Name:      "server",
			Provider:  "provider[\"registry.terraform.io/hashicorp/aws\"]",
			Instances: []hcp.StateInstance{{}},
		},
	}

	counts := CountResources(resources)
	require.NotNil(t, counts)
	assert.Equal(t, 1, counts.Total)
	assert.Equal(t, 1, counts.Managed)
	assert.Equal(t, 0, counts.DataSources)
	assert.Equal(t, 1, counts.RUM)
}

func TestCountResources_ExplicitModule(t *testing.T) {
	resources := []hcp.StateResource{
		{
			Mode:      "managed",
			Type:      "aws_instance",
			Name:      "app",
			Module:    "module.compute",
			Provider:  "provider[\"registry.terraform.io/hashicorp/aws\"]",
			Instances: []hcp.StateInstance{{}, {}},
		},
	}

	counts := CountResources(resources)
	require.NotNil(t, counts)
	assert.Equal(t, 2, counts.ByModule["module.compute"])
}

func TestCountResources_Empty(t *testing.T) {
	counts := CountResources([]hcp.StateResource{})
	require.NotNil(t, counts)
	assert.Equal(t, 0, counts.Total)
}
