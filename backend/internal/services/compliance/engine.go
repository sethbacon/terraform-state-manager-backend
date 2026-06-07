package compliance

import (
	"encoding/json"
	"fmt"

	"github.com/terraform-state-manager/terraform-state-manager/internal/db/models"
	"github.com/terraform-state-manager/terraform-state-manager/internal/services/compliance/rules"
)

// PolicyEngine evaluates a single compliance policy against an analysis result.
//
// It is the pluggable seam for compliance evaluation: the built-in
// CustomRulesEngine dispatches to the hand-written rule checkers, and future
// engines (e.g. an OPA/Rego engine) can satisfy this interface and be
// registered alongside it.
type PolicyEngine interface {
	// Name returns the engine identifier used to look it up in the registry.
	Name() string
	// Evaluate checks the policy against the analysis result and returns the
	// compliance outcome.
	Evaluate(policy models.CompliancePolicy, result models.AnalysisResult) (*models.ComplianceResult, error)
}

// CustomRulesEngine is the built-in PolicyEngine. It dispatches to the
// hand-written rule checkers in the rules package based on policy type,
// reproducing the original Checker.checkPolicy behaviour exactly.
type CustomRulesEngine struct{}

// Name returns the registry key for the built-in engine.
func (CustomRulesEngine) Name() string {
	return "custom"
}

// Evaluate dispatches to the appropriate rule checker based on policy type.
func (CustomRulesEngine) Evaluate(policy models.CompliancePolicy, result models.AnalysisResult) (*models.ComplianceResult, error) {
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

// newEngineRegistry builds the default engine registry, keyed by engine name.
// It registers the built-in custom rules engine and the embedded OPA/Rego
// engine; the latter compiles its modules eagerly, so a build error here means
// the embedded Rego is malformed.
func newEngineRegistry() (map[string]PolicyEngine, error) {
	custom := CustomRulesEngine{}
	opa, err := NewOPAEngine()
	if err != nil {
		return nil, fmt.Errorf("failed to build OPA engine: %w", err)
	}
	return map[string]PolicyEngine{
		custom.Name(): custom,
		opa.Name():    opa,
	}, nil
}
