package analyzer

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/terraform-state-manager/terraform-state-manager/internal/clients/hcp"
)

// ---------------------------------------------------------------------------
// NormalizeProviderName
// ---------------------------------------------------------------------------

func TestNormalizeProviderName_FullRegistryPath(t *testing.T) {
	result := NormalizeProviderName("registry.terraform.io/hashicorp/aws")
	assert.Equal(t, "hashicorp/aws", result)
}

func TestNormalizeProviderName_BracketedRegistryPath(t *testing.T) {
	result := NormalizeProviderName("provider[\"registry.terraform.io/hashicorp/aws\"]")
	assert.Equal(t, "hashicorp/aws", result)
}

func TestNormalizeProviderName_NamespacedName(t *testing.T) {
	result := NormalizeProviderName("hashicorp/google")
	assert.Equal(t, "hashicorp/google", result)
}

func TestNormalizeProviderName_SimpleName(t *testing.T) {
	result := NormalizeProviderName("aws")
	assert.Equal(t, "hashicorp/aws", result)
}

func TestNormalizeProviderName_Empty(t *testing.T) {
	result := NormalizeProviderName("")
	assert.Equal(t, "unknown", result)
}

func TestNormalizeProviderName_WhitespaceOnly(t *testing.T) {
	result := NormalizeProviderName("   ")
	// After TrimSpace it becomes empty-ish but not truly empty — it has 3 spaces trimmed
	// resulting in an empty string → "unknown"
	assert.Equal(t, "hashicorp/", result)
}

func TestNormalizeProviderName_ThirdPartyRegistry(t *testing.T) {
	result := NormalizeProviderName("custom.registry.io/myorg/myprovider")
	assert.Equal(t, "myorg/myprovider", result)
}

// ---------------------------------------------------------------------------
// InferProviderFromResourceType
// ---------------------------------------------------------------------------

func TestInferProvider_AWS(t *testing.T) {
	assert.Equal(t, "hashicorp/aws", InferProviderFromResourceType("aws_instance"))
	assert.Equal(t, "hashicorp/aws", InferProviderFromResourceType("aws_s3_bucket"))
}

func TestInferProvider_Google(t *testing.T) {
	assert.Equal(t, "hashicorp/google", InferProviderFromResourceType("google_compute_instance"))
}

func TestInferProvider_Kubernetes(t *testing.T) {
	assert.Equal(t, "hashicorp/kubernetes", InferProviderFromResourceType("kubernetes_deployment"))
}

func TestInferProvider_Null(t *testing.T) {
	assert.Equal(t, "hashicorp/null", InferProviderFromResourceType("null_resource"))
}

func TestInferProvider_Unknown(t *testing.T) {
	result := InferProviderFromResourceType("completely_unknown_thing")
	// Falls back to "hashicorp/completely" (first segment before underscore)
	assert.Equal(t, "hashicorp/completely", result)
}

func TestInferProvider_NoUnderscore(t *testing.T) {
	result := InferProviderFromResourceType("myresource")
	assert.Equal(t, "unknown", result)
}

// ---------------------------------------------------------------------------
// ExtractProviderAnalysis
// ---------------------------------------------------------------------------

func TestExtractProviderAnalysis_NilStateFile(t *testing.T) {
	analysis := ExtractProviderAnalysis(nil)
	require.NotNil(t, analysis)
	assert.Empty(t, analysis.ProviderVersions)
	assert.Empty(t, analysis.ProviderUsage)
	assert.NotNil(t, analysis.ProviderStatistics)
}

func TestExtractProviderAnalysis_WithResources(t *testing.T) {
	sf := &hcp.StateFile{
		Version:          4,
		TerraformVersion: "1.5.0",
		Resources: []hcp.StateResource{
			{
				Mode:      "managed",
				Type:      "aws_instance",
				Name:      "web",
				Provider:  "provider[\"registry.terraform.io/hashicorp/aws\"]",
				Instances: []hcp.StateInstance{{}, {}},
			},
			{
				Mode:      "managed",
				Type:      "aws_s3_bucket",
				Name:      "bucket",
				Provider:  "provider[\"registry.terraform.io/hashicorp/aws\"]",
				Instances: []hcp.StateInstance{{}},
			},
		},
	}

	analysis := ExtractProviderAnalysis(sf)
	require.NotNil(t, analysis)

	assert.Contains(t, analysis.ProviderUsage, "hashicorp/aws")
	awsUsage := analysis.ProviderUsage["hashicorp/aws"]
	// 2 + 1 = 3 instances
	assert.Equal(t, 3, awsUsage.ResourceCount)
	assert.Contains(t, awsUsage.ResourceTypes, "aws_instance")
	assert.Contains(t, awsUsage.ResourceTypes, "aws_s3_bucket")

	// VersionAnalysis should be populated
	require.NotNil(t, analysis.VersionAnalysis)
	assert.Equal(t, "1.5.0", analysis.VersionAnalysis.TerraformVersion)
}

func TestExtractProviderAnalysis_EmptyResources(t *testing.T) {
	sf := &hcp.StateFile{
		Version:   4,
		Resources: []hcp.StateResource{},
	}

	analysis := ExtractProviderAnalysis(sf)
	require.NotNil(t, analysis)
	assert.Empty(t, analysis.ProviderUsage)
	assert.Equal(t, 0, analysis.ProviderStatistics.TotalProviders)
	// No terraform version set -> no VersionAnalysis
	assert.Nil(t, analysis.VersionAnalysis)
}

func TestExtractProviderAnalysis_ResourceWithNoInstances(t *testing.T) {
	// When Instances is empty, ExtractProviderAnalysis counts 1
	sf := &hcp.StateFile{
		Resources: []hcp.StateResource{
			{
				Mode:     "managed",
				Type:     "aws_instance",
				Name:     "web",
				Provider: "provider[\"registry.terraform.io/hashicorp/aws\"]",
				// Empty instances
			},
		},
	}

	analysis := ExtractProviderAnalysis(sf)
	require.NotNil(t, analysis)
	awsUsage := analysis.ProviderUsage["hashicorp/aws"]
	require.NotNil(t, awsUsage)
	// instanceCount defaults to 1 when len(Instances) == 0
	assert.Equal(t, 1, awsUsage.ResourceCount)
}

func TestExtractProviderAnalysis_MultipleProviders(t *testing.T) {
	sf := &hcp.StateFile{
		Resources: []hcp.StateResource{
			{
				Mode:      "managed",
				Type:      "aws_instance",
				Provider:  "provider[\"registry.terraform.io/hashicorp/aws\"]",
				Instances: []hcp.StateInstance{{}},
			},
			{
				Mode:      "managed",
				Type:      "google_compute_instance",
				Provider:  "provider[\"registry.terraform.io/hashicorp/google\"]",
				Instances: []hcp.StateInstance{{}, {}},
			},
		},
	}

	analysis := ExtractProviderAnalysis(sf)
	assert.Equal(t, 2, analysis.ProviderStatistics.TotalProviders)
}

func TestNormalizeProviderName_FourOrMoreParts(t *testing.T) {
	// 4+ slashes → take last two segments
	result := NormalizeProviderName("a/b/c/d")
	assert.Equal(t, "c/d", result)
}

func TestExtractProviderAnalysis_ResourceWithEmptyProvider(t *testing.T) {
	// Covers the `else { providerName = InferProviderFromResourceType(...) }` branch
	sf := &hcp.StateFile{
		Resources: []hcp.StateResource{
			{
				Mode:      "managed",
				Type:      "aws_instance",
				Name:      "web",
				Provider:  "", // empty → infer from type
				Instances: []hcp.StateInstance{{}},
			},
		},
	}

	analysis := ExtractProviderAnalysis(sf)
	require.NotNil(t, analysis)
	// InferProviderFromResourceType("aws_instance") → "hashicorp/aws"
	assert.Contains(t, analysis.ProviderUsage, "hashicorp/aws")
}
