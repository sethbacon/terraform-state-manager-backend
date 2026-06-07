package compliance

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"

	"github.com/open-policy-agent/opa/v1/rego"

	"github.com/terraform-state-manager/terraform-state-manager/internal/db/models"
)

// regoModules holds the embedded Rego source for each supported policy type.
// Each module exposes a `violations` set under its own package, evaluated as
// `data.compliance.<type>.violations`.
//
//go:embed rego/*.rego
var regoModules embed.FS

// opaModule pairs an embedded Rego file with the query that yields its
// violations set.
type opaModule struct {
	file  string
	query string
}

// opaModulesByPolicyType maps each supported policy type to its Rego module.
var opaModulesByPolicyType = map[string]opaModule{
	models.PolicyTypeTagging: {file: "rego/tagging.rego", query: "data.compliance.tagging.violations"},
	models.PolicyTypeNaming:  {file: "rego/naming.rego", query: "data.compliance.naming.violations"},
	models.PolicyTypeVersion: {file: "rego/version.rego", query: "data.compliance.version.violations"},
}

// OPAEngine is a PolicyEngine backed by embedded OPA/Rego modules. For each
// supported policy type it evaluates the corresponding Rego module in-process
// and maps the resulting violations to a ComplianceResult, reproducing the
// status and violation shape of the built-in CustomRulesEngine.
type OPAEngine struct {
	preparedQueries map[string]rego.PreparedEvalQuery
}

// NewOPAEngine compiles the embedded Rego modules once and returns a ready
// engine. Compilation failures are surfaced immediately so a misbuilt module
// never silently degrades evaluation at runtime.
func NewOPAEngine() (*OPAEngine, error) {
	prepared := make(map[string]rego.PreparedEvalQuery, len(opaModulesByPolicyType))
	for policyType, mod := range opaModulesByPolicyType {
		src, err := regoModules.ReadFile(mod.file)
		if err != nil {
			return nil, fmt.Errorf("read embedded rego %q: %w", mod.file, err)
		}

		query, err := rego.New(
			rego.Query(mod.query),
			rego.Module(mod.file, string(src)),
		).PrepareForEval(context.Background())
		if err != nil {
			return nil, fmt.Errorf("prepare rego for policy type %q: %w", policyType, err)
		}
		prepared[policyType] = query
	}
	return &OPAEngine{preparedQueries: prepared}, nil
}

// Name returns the registry key for the OPA engine.
func (*OPAEngine) Name() string {
	return "opa"
}

// Evaluate selects the Rego module for the policy type, evaluates it against
// the policy config and analysis result, and maps the violations to a
// ComplianceResult with the same status mapping as the custom rules: any
// violations under a critical policy fail, otherwise they warn; no violations
// pass.
func (e *OPAEngine) Evaluate(policy models.CompliancePolicy, result models.AnalysisResult) (*models.ComplianceResult, error) {
	query, ok := e.preparedQueries[policy.PolicyType]
	if !ok {
		return nil, fmt.Errorf("unsupported policy type: %s", policy.PolicyType)
	}

	input, err := buildOPAInput(policy, result)
	if err != nil {
		return nil, err
	}

	rs, err := query.Eval(context.Background(), rego.EvalInput(input))
	if err != nil {
		return nil, fmt.Errorf("evaluate %s policy via OPA: %w", policy.PolicyType, err)
	}

	violations, err := violationsFromResultSet(rs)
	if err != nil {
		return nil, err
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

// buildOPAInput assembles the Rego input document:
//
//	{ "policy_config": <policy.Config>, "result": <analysis result> }
//
// The analysis result is round-tripped through JSON so the Rego modules see the
// same field names (resources_by_type, workspace_name, terraform_version) the
// custom rules read from the model.
func buildOPAInput(policy models.CompliancePolicy, result models.AnalysisResult) (map[string]interface{}, error) {
	var policyConfig interface{}
	if len(policy.Config) > 0 {
		if err := json.Unmarshal(policy.Config, &policyConfig); err != nil {
			return nil, fmt.Errorf("parse policy config: %w", err)
		}
	}

	resultJSON, err := json.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("marshal analysis result: %w", err)
	}
	var resultDoc interface{}
	if err := json.Unmarshal(resultJSON, &resultDoc); err != nil {
		return nil, fmt.Errorf("decode analysis result: %w", err)
	}

	return map[string]interface{}{
		"policy_config": policyConfig,
		"result":        resultDoc,
	}, nil
}

// violationsFromResultSet extracts the violations set from a Rego result set as
// a slice of maps, the same element shape the custom rules emit. An empty or
// absent set yields an empty (non-nil) slice so the marshaled JSON is "[]".
func violationsFromResultSet(rs rego.ResultSet) ([]map[string]interface{}, error) {
	violations := []map[string]interface{}{}
	if len(rs) == 0 || len(rs[0].Expressions) == 0 {
		return violations, nil
	}

	raw, ok := rs[0].Expressions[0].Value.([]interface{})
	if !ok {
		return nil, fmt.Errorf("unexpected rego violations type: %T", rs[0].Expressions[0].Value)
	}

	for _, item := range raw {
		m, ok := item.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("unexpected rego violation element type: %T", item)
		}
		violations = append(violations, m)
	}
	return violations, nil
}
