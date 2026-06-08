package compliance

import (
	"encoding/json"
	"testing"

	"github.com/terraform-state-manager/terraform-state-manager/internal/db/models"
)

// stubEngine is a PolicyEngine that records whether it was invoked, used to
// assert engine resolution routes to the engine named by policy.EngineType.
type stubEngine struct {
	name    string
	called  bool
	wantErr bool
}

func (s *stubEngine) Name() string { return s.name }

func (s *stubEngine) Evaluate(_ models.CompliancePolicy, _ models.AnalysisResult) (*models.ComplianceResult, error) {
	s.called = true
	if s.wantErr {
		return nil, errStub
	}
	return &models.ComplianceResult{Status: models.ComplianceStatusPass, Violations: json.RawMessage("[]")}, nil
}

var errStub = &stubError{}

type stubError struct{}

func (*stubError) Error() string { return "stub" }

// newCheckerWithEngines builds a Checker with a controlled engine registry.
// checkPolicy never touches the repositories, so nil repos are safe here.
func newCheckerWithEngines(engines map[string]PolicyEngine) *Checker {
	return &Checker{engines: engines}
}

func TestChecker_CheckPolicy_EngineResolution(t *testing.T) {
	tests := []struct {
		name       string
		engineType string
		wantEngine string // which stub should have been called
	}{
		{name: "explicit custom", engineType: "custom", wantEngine: "custom"},
		{name: "explicit opa", engineType: "opa", wantEngine: "opa"},
		{name: "empty falls back to custom", engineType: "", wantEngine: "custom"},
		{name: "unknown falls back to custom", engineType: "nope", wantEngine: "custom"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			custom := &stubEngine{name: "custom"}
			opa := &stubEngine{name: "opa"}
			c := newCheckerWithEngines(map[string]PolicyEngine{
				"custom": custom,
				"opa":    opa,
			})

			policy := models.CompliancePolicy{
				PolicyType: models.PolicyTypeTagging,
				EngineType: tt.engineType,
				Config:     json.RawMessage(`{"required_tags":[]}`),
			}

			if _, err := c.checkPolicy(policy, models.AnalysisResult{WorkspaceName: "prod"}); err != nil {
				t.Fatalf("checkPolicy() error: %v", err)
			}

			engines := map[string]*stubEngine{"custom": custom, "opa": opa}
			for name, eng := range engines {
				wantCalled := name == tt.wantEngine
				if eng.called != wantCalled {
					t.Errorf("engine %q called = %v, want %v", name, eng.called, wantCalled)
				}
			}
		})
	}
}

// TestChecker_CheckPolicy_NoEnginesRegistered ensures a checker with an empty
// registry surfaces an error rather than panicking.
func TestChecker_CheckPolicy_NoEnginesRegistered(t *testing.T) {
	c := newCheckerWithEngines(map[string]PolicyEngine{})
	_, err := c.checkPolicy(models.CompliancePolicy{PolicyType: models.PolicyTypeTagging}, models.AnalysisResult{})
	if err == nil {
		t.Fatal("checkPolicy() expected error with empty engine registry, got nil")
	}
}

// TestNewChecker_RegistersBothEngines proves the production constructor wires
// up both the custom and OPA engines.
func TestNewChecker_RegistersBothEngines(t *testing.T) {
	c := NewChecker(nil, nil)
	for _, name := range []string{"custom", "opa"} {
		if _, ok := c.engines[name]; !ok {
			t.Errorf("expected engine %q to be registered", name)
		}
	}
}

// TestChecker_EngineNames returns the registered engine names sorted.
func TestChecker_EngineNames(t *testing.T) {
	c := newCheckerWithEngines(map[string]PolicyEngine{
		"opa":    &stubEngine{name: "opa"},
		"custom": &stubEngine{name: "custom"},
	})
	got := c.EngineNames()
	want := []string{"custom", "opa"}
	if len(got) != len(want) {
		t.Fatalf("EngineNames() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("EngineNames()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestChecker_HasEngine reports membership against the registry.
func TestChecker_HasEngine(t *testing.T) {
	c := newCheckerWithEngines(map[string]PolicyEngine{
		"custom": &stubEngine{name: "custom"},
	})
	if !c.HasEngine("custom") {
		t.Error("HasEngine(\"custom\") = false, want true")
	}
	if c.HasEngine("opa") {
		t.Error("HasEngine(\"opa\") = true, want false")
	}
}
