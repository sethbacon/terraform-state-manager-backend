package analysis

import (
	"encoding/json"
	"io"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/terraform-state-manager/terraform-state-manager/internal/db/models"
	"github.com/terraform-state-manager/terraform-state-manager/internal/services/analyzer"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestParseRunRepoMetadata_FromConfig(t *testing.T) {
	repoMeta := map[string]interface{}{
		"required_version": ">= 1.5.0, < 2.0.0",
		"lock_file": `provider "registry.terraform.io/hashicorp/aws" {
  version     = "5.31.0"
  constraints = ">= 5.0.0"
}`,
	}
	cfg, err := json.Marshal(map[string]interface{}{"repo_metadata": repoMeta})
	require.NoError(t, err)

	res := parseRunRepoMetadata(cfg, discardLogger())
	require.NotNil(t, res)
	assert.Equal(t, ">= 1.5.0, < 2.0.0", res.RequiredVersionSpec)
	require.Len(t, res.ProviderLockPins, 1)
	assert.Equal(t, "5.31.0", res.ProviderLockPins[0].Version)
}

func TestParseRunRepoMetadata_NoMetadata(t *testing.T) {
	assert.Nil(t, parseRunRepoMetadata(nil, discardLogger()))
	assert.Nil(t, parseRunRepoMetadata(json.RawMessage(`{}`), discardLogger()))
	assert.Nil(t, parseRunRepoMetadata(json.RawMessage(`{"repo_metadata":{}}`), discardLogger()))
}

func TestApplyRepoMetadata_FlagsDrift(t *testing.T) {
	actual := "1.4.0"
	result := &models.AnalysisResult{TerraformVersion: &actual}

	repoAnalysis := &analyzer.RepoMetadataAnalysis{
		RequiredVersionSpec: ">= 1.5.0",
		ProviderLockPins: []analyzer.ProviderLockPin{
			{Source: "registry.terraform.io/hashicorp/aws", Version: "5.31.0"},
		},
	}

	applyRepoMetadata(result, repoAnalysis)

	require.NotNil(t, result.RequiredVersionSpec)
	assert.Equal(t, ">= 1.5.0", *result.RequiredVersionSpec)
	require.NotEmpty(t, result.ProviderLockPins)
	require.NotEmpty(t, result.VersionDriftReport)

	var report analyzer.VersionDriftReport
	require.NoError(t, json.Unmarshal(result.VersionDriftReport, &report))
	assert.Equal(t, ">= 1.5.0", report.Required)
	assert.Equal(t, "1.4.0", report.Actual)
	assert.False(t, report.Satisfies)
	assert.Equal(t, analyzer.DriftStatusDrift, report.Status)
}

func TestApplyRepoMetadata_Satisfied(t *testing.T) {
	actual := "1.7.2"
	result := &models.AnalysisResult{TerraformVersion: &actual}

	applyRepoMetadata(result, &analyzer.RepoMetadataAnalysis{RequiredVersionSpec: ">= 1.5.0"})

	require.NotEmpty(t, result.VersionDriftReport)
	var report analyzer.VersionDriftReport
	require.NoError(t, json.Unmarshal(result.VersionDriftReport, &report))
	assert.True(t, report.Satisfies)
	assert.Equal(t, analyzer.DriftStatusSatisfied, report.Status)
}

func TestApplyRepoMetadata_Nil(t *testing.T) {
	actual := "1.4.0"
	result := &models.AnalysisResult{TerraformVersion: &actual}
	applyRepoMetadata(result, nil)
	assert.Nil(t, result.RequiredVersionSpec)
	assert.Empty(t, result.VersionDriftReport)
}

func TestApplyRepoMetadata_UnknownWhenNoActual(t *testing.T) {
	// No in-state terraform_version → drift status is "unknown".
	result := &models.AnalysisResult{}
	applyRepoMetadata(result, &analyzer.RepoMetadataAnalysis{RequiredVersionSpec: ">= 1.5.0"})

	require.NotEmpty(t, result.VersionDriftReport)
	var report analyzer.VersionDriftReport
	require.NoError(t, json.Unmarshal(result.VersionDriftReport, &report))
	assert.Equal(t, analyzer.DriftStatusUnknown, report.Status)
}
