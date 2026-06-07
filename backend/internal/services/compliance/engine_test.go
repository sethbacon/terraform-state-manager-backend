package compliance

import (
	"encoding/json"
	"sort"
	"testing"

	"github.com/terraform-state-manager/terraform-state-manager/internal/db/models"
	"github.com/terraform-state-manager/terraform-state-manager/internal/services/compliance/rules"
)

func strptr(s string) *string { return &s }

// violationsEqualUnordered reports whether two violations JSON arrays contain
// the same elements regardless of order. CheckTagging builds violations while
// ranging over a map, so Go's randomized map iteration can reorder them between
// two identical calls; only inter-element order needs normalizing (encoding/json
// already sorts object keys deterministically).
func violationsEqualUnordered(t *testing.T, a, b json.RawMessage) bool {
	t.Helper()
	norm := func(raw json.RawMessage) []string {
		var items []map[string]interface{}
		if err := json.Unmarshal(raw, &items); err != nil {
			t.Fatalf("unmarshal violations %q: %v", string(raw), err)
		}
		out := make([]string, 0, len(items))
		for _, it := range items {
			enc, err := json.Marshal(it)
			if err != nil {
				t.Fatalf("marshal violation: %v", err)
			}
			out = append(out, string(enc))
		}
		sort.Strings(out)
		return out
	}
	na, nb := norm(a), norm(b)
	if len(na) != len(nb) {
		return false
	}
	for i := range na {
		if na[i] != nb[i] {
			return false
		}
	}
	return true
}

// resourcesByTypeJSON builds a resources_by_type JSONB value with the given
// resource types, each carrying the provided tags map (or no tags when nil).
func resourcesByTypeJSON(t *testing.T, byType map[string]map[string]string) json.RawMessage {
	t.Helper()
	out := map[string]interface{}{}
	for resourceType, tags := range byType {
		entry := map[string]interface{}{}
		if tags != nil {
			tagMap := map[string]interface{}{}
			for k, v := range tags {
				tagMap[k] = v
			}
			entry["tags"] = tagMap
		}
		out[resourceType] = entry
	}
	b, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal resources_by_type: %v", err)
	}
	return b
}

func TestCustomRulesEngine_Name(t *testing.T) {
	if got := (CustomRulesEngine{}).Name(); got != "custom" {
		t.Fatalf("Name() = %q, want %q", got, "custom")
	}
}

func TestCustomRulesEngine_Evaluate(t *testing.T) {
	tests := []struct {
		name           string
		policy         models.CompliancePolicy
		result         models.AnalysisResult
		wantErr        bool
		wantStatus     string
		wantViolations bool // true if at least one violation expected
	}{
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
			wantStatus:     models.ComplianceStatusPass,
			wantViolations: false,
		},
		{
			name: "tagging violation - missing required tag, critical fails",
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
			wantStatus:     models.ComplianceStatusFail,
			wantViolations: true,
		},
		{
			name: "naming pass - workspace matches pattern",
			policy: models.CompliancePolicy{
				PolicyType: models.PolicyTypeNaming,
				Severity:   models.AlertSeverityWarning,
				Config:     json.RawMessage(`{"workspace_pattern":"^prod-"}`),
			},
			result: models.AnalysisResult{
				WorkspaceName: "prod-east",
			},
			wantStatus:     models.ComplianceStatusPass,
			wantViolations: false,
		},
		{
			name: "naming violation - workspace fails pattern, warning",
			policy: models.CompliancePolicy{
				PolicyType: models.PolicyTypeNaming,
				Severity:   models.AlertSeverityWarning,
				Config:     json.RawMessage(`{"workspace_pattern":"^prod-"}`),
			},
			result: models.AnalysisResult{
				WorkspaceName: "dev-east",
			},
			wantStatus:     models.ComplianceStatusWarning,
			wantViolations: true,
		},
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
			wantStatus:     models.ComplianceStatusPass,
			wantViolations: false,
		},
		{
			name: "version violation - below minimum, critical fails",
			policy: models.CompliancePolicy{
				PolicyType: models.PolicyTypeVersion,
				Severity:   models.AlertSeverityCritical,
				Config:     json.RawMessage(`{"min_version":"1.5.0"}`),
			},
			result: models.AnalysisResult{
				WorkspaceName:    "prod",
				TerraformVersion: strptr("1.3.0"),
			},
			wantStatus:     models.ComplianceStatusFail,
			wantViolations: true,
		},
		{
			name: "custom - always passes",
			policy: models.CompliancePolicy{
				PolicyType: models.PolicyTypeCustom,
				Severity:   models.AlertSeverityCritical,
				Config:     json.RawMessage(`{"anything":"ignored"}`),
			},
			result: models.AnalysisResult{
				WorkspaceName: "prod",
			},
			wantStatus:     models.ComplianceStatusPass,
			wantViolations: false,
		},
		{
			name: "unknown policy type returns error",
			policy: models.CompliancePolicy{
				PolicyType: "does-not-exist",
				Config:     json.RawMessage(`{}`),
			},
			result: models.AnalysisResult{
				WorkspaceName: "prod",
			},
			wantErr: true,
		},
	}

	engine := CustomRulesEngine{}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := engine.Evaluate(tt.policy, tt.result)

			if tt.wantErr {
				if err == nil {
					t.Fatalf("Evaluate() expected error, got nil (result=%+v)", got)
				}
				if got != nil {
					t.Fatalf("Evaluate() returned non-nil result alongside error: %+v", got)
				}
				return
			}

			if err != nil {
				t.Fatalf("Evaluate() unexpected error: %v", err)
			}
			if got == nil {
				t.Fatalf("Evaluate() returned nil result without error")
			}
			if got.Status != tt.wantStatus {
				t.Errorf("Status = %q, want %q", got.Status, tt.wantStatus)
			}

			var violations []map[string]interface{}
			if err := json.Unmarshal(got.Violations, &violations); err != nil {
				t.Fatalf("violations is not valid JSON array: %v", err)
			}
			if tt.wantViolations && len(violations) == 0 {
				t.Errorf("expected at least one violation, got none")
			}
			if !tt.wantViolations && len(violations) != 0 {
				t.Errorf("expected no violations, got %d: %s", len(violations), string(got.Violations))
			}
		})
	}
}

// TestCustomRulesEngine_ParityWithRules proves zero behaviour change: for each
// built-in rule type, CustomRulesEngine.Evaluate produces the exact same status
// and violation bytes as calling the rules.CheckX function directly.
func TestCustomRulesEngine_ParityWithRules(t *testing.T) {
	type ruleFn func(models.CompliancePolicy, models.AnalysisResult) (*models.ComplianceResult, error)

	taggingResult := models.AnalysisResult{
		WorkspaceName: "prod",
		ResourcesByType: resourcesByTypeJSON(t, map[string]map[string]string{
			"aws_instance":  {"env": "prod"},
			"aws_s3_bucket": nil,
		}),
	}

	tests := []struct {
		name   string
		policy models.CompliancePolicy
		result models.AnalysisResult
		direct ruleFn
	}{
		{
			name: "tagging",
			policy: models.CompliancePolicy{
				PolicyType: models.PolicyTypeTagging,
				Severity:   models.AlertSeverityCritical,
				Config:     json.RawMessage(`{"required_tags":["env","owner"]}`),
			},
			result: taggingResult,
			direct: rules.CheckTagging,
		},
		{
			name: "naming",
			policy: models.CompliancePolicy{
				PolicyType: models.PolicyTypeNaming,
				Severity:   models.AlertSeverityWarning,
				Config:     json.RawMessage(`{"workspace_pattern":"^prod-"}`),
			},
			result: models.AnalysisResult{WorkspaceName: "dev-east"},
			direct: rules.CheckNaming,
		},
		{
			name: "version",
			policy: models.CompliancePolicy{
				PolicyType: models.PolicyTypeVersion,
				Severity:   models.AlertSeverityCritical,
				Config:     json.RawMessage(`{"min_version":"1.5.0"}`),
			},
			result: models.AnalysisResult{
				WorkspaceName:    "prod",
				TerraformVersion: strptr("1.3.0"),
			},
			direct: rules.CheckVersion,
		},
	}

	engine := CustomRulesEngine{}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			viaEngine, errEngine := engine.Evaluate(tt.policy, tt.result)
			viaDirect, errDirect := tt.direct(tt.policy, tt.result)

			if (errEngine == nil) != (errDirect == nil) {
				t.Fatalf("error mismatch: engine=%v direct=%v", errEngine, errDirect)
			}
			if errEngine != nil {
				return
			}

			if viaEngine.Status != viaDirect.Status {
				t.Errorf("status mismatch: engine=%q direct=%q", viaEngine.Status, viaDirect.Status)
			}
			if !violationsEqualUnordered(t, viaEngine.Violations, viaDirect.Violations) {
				t.Errorf("violations bytes mismatch:\n engine=%s\n direct=%s",
					string(viaEngine.Violations), string(viaDirect.Violations))
			}
		})
	}
}
