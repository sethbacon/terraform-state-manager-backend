package rules

import (
	"encoding/json"
	"fmt"
	"regexp"

	"github.com/terraform-state-manager/terraform-state-manager/internal/db/models"
)

// namingConfig defines the expected configuration for naming compliance policies.
type namingConfig struct {
	WorkspacePattern string `json:"workspace_pattern"`
	ResourcePattern  string `json:"resource_pattern"`
}

// CheckNaming checks workspace and resource names against naming patterns in the policy config.
func CheckNaming(policy models.CompliancePolicy, result models.AnalysisResult) (*models.ComplianceResult, error) {
	var cfg namingConfig
	if err := json.Unmarshal(policy.Config, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse naming policy config: %w", err)
	}

	var violations []map[string]interface{}

	// Check workspace name against pattern.
	if cfg.WorkspacePattern != "" {
		re, err := regexp.Compile(cfg.WorkspacePattern)
		if err != nil {
			return nil, fmt.Errorf("invalid workspace naming pattern: %w", err)
		}

		if !re.MatchString(result.WorkspaceName) {
			violations = append(violations, map[string]interface{}{
				"type":    "workspace_name",
				"name":    result.WorkspaceName,
				"pattern": cfg.WorkspacePattern,
				"message": fmt.Sprintf("Workspace name '%s' does not match required pattern: %s", result.WorkspaceName, cfg.WorkspacePattern),
			})
		}
	}

	// Check resource names against pattern.
	if cfg.ResourcePattern != "" {
		re, err := regexp.Compile(cfg.ResourcePattern)
		if err != nil {
			return nil, fmt.Errorf("invalid resource naming pattern: %w", err)
		}

		var resourcesByType map[string]interface{}
		if len(result.ResourcesByType) > 0 {
			_ = json.Unmarshal(result.ResourcesByType, &resourcesByType)
		}

		for resourceName := range resourcesByType {
			if !re.MatchString(resourceName) {
				violations = append(violations, map[string]interface{}{
					"type":    "resource_name",
					"name":    resourceName,
					"pattern": cfg.ResourcePattern,
					"message": fmt.Sprintf("Resource name '%s' does not match required pattern: %s", resourceName, cfg.ResourcePattern),
				})
			}
		}
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
