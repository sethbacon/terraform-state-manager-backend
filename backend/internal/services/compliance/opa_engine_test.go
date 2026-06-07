package compliance

import (
	"encoding/json"
	"testing"

	"github.com/terraform-state-manager/terraform-state-manager/internal/db/models"
)

func TestOPAEngine_Name(t *testing.T) {
	engine, err := NewOPAEngine()
	if err != nil {
		t.Fatalf("NewOPAEngine() error: %v", err)
	}
	if got := engine.Name(); got != "opa" {
		t.Fatalf("Name() = %q, want %q", got, "opa")
	}
}

// TestOPAEngine_CrossEngineParity is the Phase 5 DONE-WHEN check: for a
// representative set of tagging/naming/version policies and analysis results,
// the embedded OPA/Rego engine and the built-in CustomRulesEngine produce
// unified results — identical Status and equivalent Violations (compared
// order-insensitively, since the tagging rule emits violations in map order).
func TestOPAEngine_CrossEngineParity(t *testing.T) {
	custom := CustomRulesEngine{}
	opa, err := NewOPAEngine()
	if err != nil {
		t.Fatalf("NewOPAEngine() error: %v", err)
	}

	tests := []struct {
		name   string
		policy models.CompliancePolicy
		result models.AnalysisResult
	}{
		// --- tagging ---
		{
			name: "tagging pass - all required tags present",
			policy: models.CompliancePolicy{
				PolicyType: models.PolicyTypeTagging,
				Severity:   models.AlertSeverityCritical,
				Config:     json.RawMessage(`{"required_tags":["env"]}`),
			},
			result: models.AnalysisResult{
				WorkspaceName: "prod",
				ResourcesByType: resourcesByTypeJSON(t, map[string]map[string]string{
					"aws_instance": {"env": "prod"},
				}),
			},
		},
		{
			name: "tagging fail - missing tag, critical",
			policy: models.CompliancePolicy{
				PolicyType: models.PolicyTypeTagging,
				Severity:   models.AlertSeverityCritical,
				Config:     json.RawMessage(`{"required_tags":["owner"]}`),
			},
			result: models.AnalysisResult{
				WorkspaceName: "prod",
				ResourcesByType: resourcesByTypeJSON(t, map[string]map[string]string{
					"aws_instance": {"env": "prod"},
				}),
			},
		},
		{
			name: "tagging warning - missing tag, non-critical",
			policy: models.CompliancePolicy{
				PolicyType: models.PolicyTypeTagging,
				Severity:   models.AlertSeverityWarning,
				Config:     json.RawMessage(`{"required_tags":["owner"]}`),
			},
			result: models.AnalysisResult{
				WorkspaceName: "prod",
				ResourcesByType: resourcesByTypeJSON(t, map[string]map[string]string{
					"aws_instance": {"env": "prod"},
				}),
			},
		},
		{
			name: "tagging multiple types and tags - mix of present, missing key, and no tags object",
			policy: models.CompliancePolicy{
				PolicyType: models.PolicyTypeTagging,
				Severity:   models.AlertSeverityCritical,
				Config:     json.RawMessage(`{"required_tags":["env","owner"]}`),
			},
			result: models.AnalysisResult{
				WorkspaceName: "prod",
				ResourcesByType: resourcesByTypeJSON(t, map[string]map[string]string{
					"aws_instance":  {"env": "prod"},
					"aws_s3_bucket": nil,
					"aws_vpc":       {"env": "prod", "owner": "team-a"},
				}),
			},
		},
		{
			name: "tagging empty required_tags - always pass",
			policy: models.CompliancePolicy{
				PolicyType: models.PolicyTypeTagging,
				Severity:   models.AlertSeverityCritical,
				Config:     json.RawMessage(`{"required_tags":[]}`),
			},
			result: models.AnalysisResult{
				WorkspaceName: "prod",
				ResourcesByType: resourcesByTypeJSON(t, map[string]map[string]string{
					"aws_instance": nil,
				}),
			},
		},
		{
			name: "tagging empty resources_by_type",
			policy: models.CompliancePolicy{
				PolicyType: models.PolicyTypeTagging,
				Severity:   models.AlertSeverityCritical,
				Config:     json.RawMessage(`{"required_tags":["env"]}`),
			},
			result: models.AnalysisResult{WorkspaceName: "prod"},
		},
		// --- naming ---
		{
			name: "naming pass - workspace matches",
			policy: models.CompliancePolicy{
				PolicyType: models.PolicyTypeNaming,
				Severity:   models.AlertSeverityWarning,
				Config:     json.RawMessage(`{"workspace_pattern":"^prod-"}`),
			},
			result: models.AnalysisResult{WorkspaceName: "prod-east"},
		},
		{
			name: "naming warning - workspace fails, non-critical",
			policy: models.CompliancePolicy{
				PolicyType: models.PolicyTypeNaming,
				Severity:   models.AlertSeverityWarning,
				Config:     json.RawMessage(`{"workspace_pattern":"^prod-"}`),
			},
			result: models.AnalysisResult{WorkspaceName: "dev-east"},
		},
		{
			name: "naming fail - workspace fails, critical",
			policy: models.CompliancePolicy{
				PolicyType: models.PolicyTypeNaming,
				Severity:   models.AlertSeverityCritical,
				Config:     json.RawMessage(`{"workspace_pattern":"^prod-"}`),
			},
			result: models.AnalysisResult{WorkspaceName: "dev-east"},
		},
		{
			name: "naming resource pattern - some resource names fail",
			policy: models.CompliancePolicy{
				PolicyType: models.PolicyTypeNaming,
				Severity:   models.AlertSeverityWarning,
				Config:     json.RawMessage(`{"resource_pattern":"^aws_"}`),
			},
			result: models.AnalysisResult{
				WorkspaceName: "prod",
				ResourcesByType: resourcesByTypeJSON(t, map[string]map[string]string{
					"aws_instance":   nil,
					"google_compute": nil,
				}),
			},
		},
		{
			name: "naming both patterns",
			policy: models.CompliancePolicy{
				PolicyType: models.PolicyTypeNaming,
				Severity:   models.AlertSeverityCritical,
				Config:     json.RawMessage(`{"workspace_pattern":"^prod-","resource_pattern":"^aws_"}`),
			},
			result: models.AnalysisResult{
				WorkspaceName: "dev-east",
				ResourcesByType: resourcesByTypeJSON(t, map[string]map[string]string{
					"azurerm_vm": nil,
				}),
			},
		},
		{
			name: "naming empty config - pass",
			policy: models.CompliancePolicy{
				PolicyType: models.PolicyTypeNaming,
				Severity:   models.AlertSeverityCritical,
				Config:     json.RawMessage(`{}`),
			},
			result: models.AnalysisResult{WorkspaceName: "anything"},
		},
		// --- version ---
		{
			name: "version pass - meets minimum",
			policy: models.CompliancePolicy{
				PolicyType: models.PolicyTypeVersion,
				Severity:   models.AlertSeverityCritical,
				Config:     json.RawMessage(`{"min_version":"1.5.0"}`),
			},
			result: models.AnalysisResult{
				WorkspaceName:    "prod",
				TerraformVersion: strptr("1.6.2"),
			},
		},
		{
			name: "version fail - below minimum, critical",
			policy: models.CompliancePolicy{
				PolicyType: models.PolicyTypeVersion,
				Severity:   models.AlertSeverityCritical,
				Config:     json.RawMessage(`{"min_version":"1.5.0"}`),
			},
			result: models.AnalysisResult{
				WorkspaceName:    "prod",
				TerraformVersion: strptr("1.3.0"),
			},
		},
		{
			name: "version warning - below minimum, non-critical",
			policy: models.CompliancePolicy{
				PolicyType: models.PolicyTypeVersion,
				Severity:   models.AlertSeverityWarning,
				Config:     json.RawMessage(`{"min_version":"1.5.0"}`),
			},
			result: models.AnalysisResult{
				WorkspaceName:    "prod",
				TerraformVersion: strptr("0.14.11"),
			},
		},
		{
			name: "version unknown - no version reported",
			policy: models.CompliancePolicy{
				PolicyType: models.PolicyTypeVersion,
				Severity:   models.AlertSeverityCritical,
				Config:     json.RawMessage(`{"min_version":"1.5.0"}`),
			},
			result: models.AnalysisResult{WorkspaceName: "prod"},
		},
		{
			name: "version with leading v and prerelease suffix",
			policy: models.CompliancePolicy{
				PolicyType: models.PolicyTypeVersion,
				Severity:   models.AlertSeverityCritical,
				Config:     json.RawMessage(`{"min_version":"v1.5.0"}`),
			},
			result: models.AnalysisResult{
				WorkspaceName:    "prod",
				TerraformVersion: strptr("v1.6.0-beta1"),
			},
		},
		{
			name: "version equal to minimum - pass",
			policy: models.CompliancePolicy{
				PolicyType: models.PolicyTypeVersion,
				Severity:   models.AlertSeverityCritical,
				Config:     json.RawMessage(`{"min_version":"1.5.0"}`),
			},
			result: models.AnalysisResult{
				WorkspaceName:    "prod",
				TerraformVersion: strptr("1.5.0"),
			},
		},
		{
			name: "version empty min_version - pass",
			policy: models.CompliancePolicy{
				PolicyType: models.PolicyTypeVersion,
				Severity:   models.AlertSeverityCritical,
				Config:     json.RawMessage(`{"min_version":""}`),
			},
			result: models.AnalysisResult{
				WorkspaceName:    "prod",
				TerraformVersion: strptr("0.1.0"),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			viaCustom, errCustom := custom.Evaluate(tt.policy, tt.result)
			viaOPA, errOPA := opa.Evaluate(tt.policy, tt.result)

			if (errCustom == nil) != (errOPA == nil) {
				t.Fatalf("error mismatch: custom=%v opa=%v", errCustom, errOPA)
			}
			if errCustom != nil {
				return
			}

			if viaOPA.Status != viaCustom.Status {
				t.Errorf("status mismatch: opa=%q custom=%q", viaOPA.Status, viaCustom.Status)
			}
			if !violationsEqualUnordered(t, viaOPA.Violations, viaCustom.Violations) {
				t.Errorf("violations mismatch:\n  opa=%s\n  custom=%s",
					string(viaOPA.Violations), string(viaCustom.Violations))
			}
		})
	}
}

// TestOPAEngine_UnsupportedPolicyType verifies the OPA engine reports an error
// for policy types it has no module for, mirroring the custom engine's handling
// of unknown types.
func TestOPAEngine_UnsupportedPolicyType(t *testing.T) {
	opa, err := NewOPAEngine()
	if err != nil {
		t.Fatalf("NewOPAEngine() error: %v", err)
	}

	policy := models.CompliancePolicy{
		PolicyType: "does-not-exist",
		Config:     json.RawMessage(`{}`),
	}
	got, err := opa.Evaluate(policy, models.AnalysisResult{WorkspaceName: "prod"})
	if err == nil {
		t.Fatalf("Evaluate() expected error for unknown policy type, got result %+v", got)
	}
	if got != nil {
		t.Fatalf("Evaluate() returned non-nil result alongside error: %+v", got)
	}
}
