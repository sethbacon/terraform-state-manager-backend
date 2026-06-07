package analyzer

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestComputeVersionDrift(t *testing.T) {
	tests := []struct {
		name          string
		required      string
		actual        string
		wantSatisfies bool
		wantStatus    string
	}{
		// satisfied cases
		{"floor met exactly", ">= 1.5.0", "1.5.0", true, DriftStatusSatisfied},
		{"floor exceeded", ">= 1.5.0", "1.7.2", true, DriftStatusSatisfied},
		{"range satisfied", ">= 1.5.0, < 2.0.0", "1.9.0", true, DriftStatusSatisfied},
		{"exact match bare", "1.5.0", "1.5.0", true, DriftStatusSatisfied},
		{"pessimistic minor satisfied", "~> 1.5", "1.9.3", true, DriftStatusSatisfied},
		{"pessimistic patch satisfied", "~> 1.5.0", "1.5.9", true, DriftStatusSatisfied},
		{"not-equal satisfied", "!= 1.4.0", "1.5.0", true, DriftStatusSatisfied},
		{"v-prefixed actual", ">= 1.5.0", "v1.6.0", true, DriftStatusSatisfied},

		// drift cases (does-not-satisfy)
		{"below floor", ">= 1.5.0", "1.4.0", false, DriftStatusDrift},
		{"above ceiling", ">= 1.5.0, < 2.0.0", "2.1.0", false, DriftStatusDrift},
		{"exact mismatch", "1.5.0", "1.6.0", false, DriftStatusDrift},
		{"pessimistic minor exceeded", "~> 1.5", "2.0.0", false, DriftStatusDrift},
		{"pessimistic patch exceeded", "~> 1.5.0", "1.6.0", false, DriftStatusDrift},
		{"not-equal violated", "!= 1.5.0", "1.5.0", false, DriftStatusDrift},

		// unknown cases
		{"missing required", "", "1.5.0", false, DriftStatusUnknown},
		{"missing actual", ">= 1.5.0", "", false, DriftStatusUnknown},
		{"both missing", "", "", false, DriftStatusUnknown},
		{"unparseable constraint", "latest", "1.5.0", false, DriftStatusUnknown},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			report := ComputeVersionDrift(tt.required, tt.actual)
			assert.Equal(t, tt.wantSatisfies, report.Satisfies, "satisfies")
			assert.Equal(t, tt.wantStatus, report.Status, "status")
			assert.Equal(t, tt.required, report.Required)
			assert.Equal(t, tt.actual, report.Actual)
		})
	}
}

func TestComputeVersionDrift_TrimsWhitespace(t *testing.T) {
	report := ComputeVersionDrift("  >= 1.5.0  ", "  1.6.0  ")
	assert.Equal(t, ">= 1.5.0", report.Required)
	assert.Equal(t, "1.6.0", report.Actual)
	assert.True(t, report.Satisfies)
	assert.Equal(t, DriftStatusSatisfied, report.Status)
}

func TestCompareVersions(t *testing.T) {
	tests := []struct {
		a, b string
		want int
	}{
		{"1.5.0", "1.5.0", 0},
		{"1.4.0", "1.5.0", -1},
		{"1.6.0", "1.5.0", 1},
		{"1.5", "1.5.0", 0},
		{"v2.0.0", "2.0.0", 0},
		{"1.10.0", "1.9.0", 1},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, compareVersions(tt.a, tt.b), "%s vs %s", tt.a, tt.b)
	}
}
