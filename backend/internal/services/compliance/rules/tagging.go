// Package rules implements individual compliance rule checkers.
package rules

import (
	"encoding/json"
	"fmt"

	"github.com/terraform-state-manager/terraform-state-manager/internal/db/models"
)

// taggingConfig defines the expected configuration for tagging compliance policies.
type taggingConfig struct {
	RequiredTags []string `json:"required_tags"`
}

// CheckTagging checks that resources have the required tags defined in the policy config.
// It inspects the resource metadata from the analysis result for missing tags.
func CheckTagging(policy models.CompliancePolicy, result models.AnalysisResult) (*models.ComplianceResult, error) {
	var cfg taggingConfig
	if err := json.Unmarshal(policy.Config, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse tagging policy config: %w", err)
	}

	if len(cfg.RequiredTags) == 0 {
		return &models.ComplianceResult{
			Status:     models.ComplianceStatusPass,
			Violations: json.RawMessage("[]"),
		}, nil
	}

	// Parse resources_by_type to inspect resource metadata for tag information.
	var resourcesByType map[string]interface{}
	if len(result.ResourcesByType) > 0 {
		_ = json.Unmarshal(result.ResourcesByType, &resourcesByType)
	}

	var violations []map[string]interface{}

	// Check each resource type for missing tags.
	for resourceType, resourceData := range resourcesByType {
		resourceMap, ok := resourceData.(map[string]interface{})
		if !ok {
			continue
		}

		tags, _ := resourceMap["tags"].(map[string]interface{})

		for _, requiredTag := range cfg.RequiredTags {
			if tags == nil {
				violations = append(violations, map[string]interface{}{
					"resource_type": resourceType,
					"missing_tag":   requiredTag,
					"message":       fmt.Sprintf("Resource type %s is missing required tag: %s", resourceType, requiredTag),
				})
				continue
			}
			if _, exists := tags[requiredTag]; !exists {
				violations = append(violations, map[string]interface{}{
					"resource_type": resourceType,
					"missing_tag":   requiredTag,
					"message":       fmt.Sprintf("Resource type %s is missing required tag: %s", resourceType, requiredTag),
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
