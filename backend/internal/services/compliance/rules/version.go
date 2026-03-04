package rules

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/terraform-state-manager/terraform-state-manager/internal/db/models"
)

// versionConfig defines the expected configuration for version compliance policies.
type versionConfig struct {
	MinVersion string `json:"min_version"`
}

// CheckVersion checks the terraform_version from the analysis result against the
// minimum version defined in the policy config.
func CheckVersion(policy models.CompliancePolicy, result models.AnalysisResult) (*models.ComplianceResult, error) {
	var cfg versionConfig
	if err := json.Unmarshal(policy.Config, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse version policy config: %w", err)
	}

	if cfg.MinVersion == "" {
		return &models.ComplianceResult{
			Status:     models.ComplianceStatusPass,
			Violations: json.RawMessage("[]"),
		}, nil
	}

	var violations []map[string]interface{}

	actualVersion := ""
	if result.TerraformVersion != nil {
		actualVersion = *result.TerraformVersion
	}

	if actualVersion == "" {
		violations = append(violations, map[string]interface{}{
			"type":        "version_unknown",
			"min_version": cfg.MinVersion,
			"message":     "Terraform version is unknown for this workspace",
		})
	} else if compareVersions(actualVersion, cfg.MinVersion) < 0 {
		violations = append(violations, map[string]interface{}{
			"type":           "version_outdated",
			"actual_version": actualVersion,
			"min_version":    cfg.MinVersion,
			"message":        fmt.Sprintf("Terraform version %s is below minimum required version %s", actualVersion, cfg.MinVersion),
		})
	}

	status := models.ComplianceStatusPass
	if len(violations) > 0 {
		if policy.Severity == models.AlertSeverityCritical {
			status = models.ComplianceStatusFail
		} else {
			status = models.ComplianceStatusWarning
		}
	}

	violationsJSON, err := json.Marshal(violations)
	if err != nil {
		violationsJSON = json.RawMessage("[]")
	}

	return &models.ComplianceResult{
		Status:     status,
		Violations: violationsJSON,
	}, nil
}

// compareVersions performs a simple semantic version comparison.
// Returns -1 if a < b, 0 if a == b, 1 if a > b.
func compareVersions(a, b string) int {
	aParts := parseVersionParts(a)
	bParts := parseVersionParts(b)

	for i := 0; i < 3; i++ {
		aVal := 0
		bVal := 0
		if i < len(aParts) {
			aVal = aParts[i]
		}
		if i < len(bParts) {
			bVal = bParts[i]
		}
		if aVal < bVal {
			return -1
		}
		if aVal > bVal {
			return 1
		}
	}
	return 0
}

// parseVersionParts splits a version string into integer components.
func parseVersionParts(version string) []int {
	// Strip any leading 'v' prefix.
	version = strings.TrimPrefix(version, "v")

	parts := strings.Split(version, ".")
	result := make([]int, 0, len(parts))
	for _, p := range parts {
		val := 0
		for _, c := range p {
			if c >= '0' && c <= '9' {
				val = val*10 + int(c-'0')
			} else {
				break
			}
		}
		result = append(result, val)
	}
	return result
}
