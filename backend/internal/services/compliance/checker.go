// Package compliance implements compliance policy checking against analysis results.
package compliance

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/terraform-state-manager/terraform-state-manager/internal/db/models"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
)

// Checker evaluates compliance policies against analysis results.
type Checker struct {
	policyRepo *repositories.CompliancePolicyRepository
	resultRepo *repositories.ComplianceResultRepository
	engines    map[string]PolicyEngine
}

// NewChecker creates a new compliance Checker.
//
// The engine registry includes the built-in custom rules engine and the
// embedded OPA/Rego engine. If the OPA engine fails to compile its embedded
// modules — only possible if the embedded Rego is malformed, which the tests
// guard against — the Checker logs the failure and continues with just the
// custom engine so compliance evaluation degrades rather than crashing.
func NewChecker(
	policyRepo *repositories.CompliancePolicyRepository,
	resultRepo *repositories.ComplianceResultRepository,
) *Checker {
	engines, err := newEngineRegistry()
	if err != nil {
		slog.Error("Falling back to custom-only compliance engine registry", "error", err)
		custom := CustomRulesEngine{}
		engines = map[string]PolicyEngine{custom.Name(): custom}
	}
	return &Checker{
		policyRepo: policyRepo,
		resultRepo: resultRepo,
		engines:    engines,
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

// checkPolicy resolves the policy engine from policy.EngineType and evaluates
// the policy against the analysis result. An empty or unknown engine type falls
// back to the built-in "custom" engine, so policies written before per-policy
// engine selection (or pointing at an unregistered engine) keep working.
func (c *Checker) checkPolicy(policy models.CompliancePolicy, result models.AnalysisResult) (*models.ComplianceResult, error) {
	engineType := policy.EngineType
	if _, ok := c.engines[engineType]; !ok {
		engineType = "custom"
	}
	engine, ok := c.engines[engineType]
	if !ok {
		return nil, fmt.Errorf("no compliance policy engine registered")
	}
	return engine.Evaluate(policy, result)
}
