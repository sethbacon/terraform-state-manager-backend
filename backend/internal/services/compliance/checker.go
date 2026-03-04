// Package compliance implements compliance policy checking against analysis results.
package compliance

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/terraform-state-manager/terraform-state-manager/internal/db/models"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
	"github.com/terraform-state-manager/terraform-state-manager/internal/services/compliance/rules"
)

// Checker evaluates compliance policies against analysis results.
type Checker struct {
	policyRepo *repositories.CompliancePolicyRepository
	resultRepo *repositories.ComplianceResultRepository
}

// NewChecker creates a new compliance Checker.
func NewChecker(
	policyRepo *repositories.CompliancePolicyRepository,
	resultRepo *repositories.ComplianceResultRepository,
) *Checker {
	return &Checker{
		policyRepo: policyRepo,
		resultRepo: resultRepo,
	}
}

// CheckRun evaluates all active compliance policies for an organization against
// the given analysis results and persists the compliance results.
func (c *Checker) CheckRun(ctx context.Context, orgID, runID string, results []models.AnalysisResult) error {
	policies, err := c.policyRepo.GetActiveByOrganization(ctx, orgID)
	if err != nil {
		return fmt.Errorf("failed to load active compliance policies: %w", err)
	}

	if len(policies) == 0 {
		slog.Info("No active compliance policies found", "org_id", orgID)
		return nil
	}

	for _, result := range results {
		if result.Status != models.ResultStatusSuccess {
			continue
		}

		for _, policy := range policies {
			complianceResult, err := c.checkPolicy(policy, result)
			if err != nil {
				slog.Error("Failed to check compliance policy",
					"policy_id", policy.ID,
					"workspace", result.WorkspaceName,
					"error", err,
				)
				continue
			}

			complianceResult.PolicyID = policy.ID
			complianceResult.RunID = runID
			complianceResult.WorkspaceName = result.WorkspaceName

			if err := c.resultRepo.Create(ctx, complianceResult); err != nil {
				slog.Error("Failed to save compliance result",
					"policy_id", policy.ID,
					"workspace", result.WorkspaceName,
					"error", err,
				)
				continue
			}
		}
	}

	slog.Info("Compliance check completed",
		"org_id", orgID,
		"run_id", runID,
		"policies_checked", len(policies),
		"results_checked", len(results),
	)

	return nil
}

// checkPolicy dispatches to the appropriate rule checker based on policy type.
func (c *Checker) checkPolicy(policy models.CompliancePolicy, result models.AnalysisResult) (*models.ComplianceResult, error) {
	switch policy.PolicyType {
	case models.PolicyTypeTagging:
		return rules.CheckTagging(policy, result)
	case models.PolicyTypeNaming:
		return rules.CheckNaming(policy, result)
	case models.PolicyTypeVersion:
		return rules.CheckVersion(policy, result)
	case models.PolicyTypeCustom:
		// Custom policies pass by default; extend as needed.
		return &models.ComplianceResult{
			Status:     models.ComplianceStatusPass,
			Violations: json.RawMessage("[]"),
		}, nil
	default:
		return nil, fmt.Errorf("unsupported policy type: %s", policy.PolicyType)
	}
}
