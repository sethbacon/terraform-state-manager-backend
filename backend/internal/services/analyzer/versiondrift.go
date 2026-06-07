package analyzer

import (
	"strings"
)

// Version drift status values for a VersionDriftReport.
const (
	// DriftStatusSatisfied means the in-state Terraform version satisfies the
	// declared required_version constraint.
	DriftStatusSatisfied = "satisfied"
	// DriftStatusDrift means the in-state version does NOT satisfy the
	// declared constraint (pin drift).
	DriftStatusDrift = "drift"
	// DriftStatusUnknown means drift could not be determined — either the
	// constraint or the actual version is missing or unparseable.
	DriftStatusUnknown = "unknown"
)

// VersionDriftReport is the headline output of this slice: it compares the
// Terraform `required_version` constraint declared in repo configuration
// against the `terraform_version` recorded in the workspace's state file.
type VersionDriftReport struct {
	// Required is the declared required_version constraint string (the spec).
	Required string `json:"required"`
	// Actual is the terraform_version recorded in the state file.
	Actual string `json:"actual"`
	// Satisfies is true when Actual satisfies the Required constraint.
	Satisfies bool `json:"satisfies"`
	// Status is one of DriftStatusSatisfied, DriftStatusDrift, DriftStatusUnknown.
	Status string `json:"status"`
}

// ComputeVersionDrift compares an in-state Terraform version (actual) against a
// declared required_version constraint (required) and produces a drift report.
//
// When either value is missing, or the constraint cannot be interpreted, the
// report status is "unknown" and Satisfies is false. Otherwise the status is
// "satisfied" or "drift" based on whether the actual version meets the
// constraint.
func ComputeVersionDrift(required, actual string) *VersionDriftReport {
	report := &VersionDriftReport{
		Required: strings.TrimSpace(required),
		Actual:   strings.TrimSpace(actual),
		Status:   DriftStatusUnknown,
	}

	if report.Required == "" || report.Actual == "" {
		return report
	}

	satisfies, ok := constraintSatisfied(report.Required, report.Actual)
	if !ok {
		// Constraint or version could not be parsed; leave status unknown.
		return report
	}

	report.Satisfies = satisfies
	if satisfies {
		report.Status = DriftStatusSatisfied
	} else {
		report.Status = DriftStatusDrift
	}
	return report
}

// constraintSatisfied reports whether actualVersion satisfies a Terraform
// version constraint string. A constraint is a comma-separated list of terms,
// each an optional operator followed by a version (e.g. ">= 1.5.0, < 2.0.0").
// All terms must hold. The second return value is false when the constraint or
// version could not be interpreted at all.
func constraintSatisfied(constraint, actualVersion string) (bool, bool) {
	terms := strings.Split(constraint, ",")
	parsedAny := false

	for _, raw := range terms {
		term := strings.TrimSpace(raw)
		if term == "" {
			continue
		}
		op, version := splitConstraintTerm(term)
		if version == "" || !isNumericVersion(version) || !isNumericVersion(actualVersion) {
			// Unparseable term or actual version — treat as unknown.
			return false, false
		}
		parsedAny = true
		if !termHolds(op, version, actualVersion) {
			return false, true
		}
	}

	if !parsedAny {
		return false, false
	}
	return true, true
}

// isNumericVersion reports whether the version string contains at least one
// digit, distinguishing a real version (e.g. "1.5.0") from an unparseable token
// (e.g. "latest"). A leading "v" prefix is tolerated.
func isNumericVersion(version string) bool {
	version = strings.TrimPrefix(strings.TrimSpace(version), "v")
	for _, c := range version {
		if c >= '0' && c <= '9' {
			return true
		}
	}
	return false
}

// splitConstraintTerm separates a single constraint term into its operator and
// version. A term with no explicit operator (a bare version) is treated as "=".
func splitConstraintTerm(term string) (op, version string) {
	// Operators ordered so multi-character operators are matched before their
	// single-character prefixes.
	for _, candidate := range []string{">=", "<=", "!=", "~>", "==", ">", "<", "="} {
		if strings.HasPrefix(term, candidate) {
			op = candidate
			version = strings.TrimSpace(term[len(candidate):])
			if op == "==" {
				op = "="
			}
			return op, version
		}
	}
	// Bare version, e.g. "1.5.0" — exact match.
	return "=", strings.TrimSpace(term)
}

// termHolds evaluates a single operator/version constraint term against the
// actual version.
func termHolds(op, version, actualVersion string) bool {
	cmp := compareVersions(actualVersion, version)
	switch op {
	case ">":
		return cmp > 0
	case ">=":
		return cmp >= 0
	case "<":
		return cmp < 0
	case "<=":
		return cmp <= 0
	case "!=":
		return cmp != 0
	case "=":
		return cmp == 0
	case "~>":
		return pessimisticHolds(version, actualVersion)
	default:
		return false
	}
}

// pessimisticHolds implements Terraform's "~>" pessimistic constraint operator:
// the actual version must be >= the constraint version and must not increment
// the last specified component. For "~> 1.5" the actual must be >= 1.5.0 and
// < 2.0.0; for "~> 1.5.0" it must be >= 1.5.0 and < 1.6.0.
func pessimisticHolds(constraintVersion, actualVersion string) bool {
	if compareVersions(actualVersion, constraintVersion) < 0 {
		return false
	}

	parts := parseVersionParts(constraintVersion)
	if len(parts) == 0 {
		return false
	}

	// The upper bound increments the second-to-last specified component when
	// two or more components are given (~> 1.5 -> < 2.0, ~> 1.5.0 -> < 1.6.0).
	// With a single component (~> 1) there is no upper bound beyond the major.
	upper := make([]int, len(parts))
	copy(upper, parts)
	if len(upper) >= 2 {
		upper[len(upper)-1] = 0
		upper[len(upper)-2]++
	} else {
		upper[0]++
	}

	return compareVersionParts(parseVersionParts(actualVersion), upper) < 0
}

// ---------------------------------------------------------------------------
// Version comparator.
//
// Mirrored (copied) from internal/services/compliance/rules/version.go per the
// repo's "extract a shared helper only after two consumers" convention. Do not
// cross-import the compliance rules package here.
// ---------------------------------------------------------------------------

// compareVersions performs a simple semantic version comparison.
// Returns -1 if a < b, 0 if a == b, 1 if a > b.
func compareVersions(a, b string) int {
	return compareVersionParts(parseVersionParts(a), parseVersionParts(b))
}

// compareVersionParts compares two already-parsed version component slices.
func compareVersionParts(aParts, bParts []int) int {
	n := len(aParts)
	if len(bParts) > n {
		n = len(bParts)
	}
	for i := 0; i < n; i++ {
		aVal := 0
		bVal := 0
		if i < len(aParts) {
			aVal = aParts[i]
		}
		if i < len(bParts) {
			bVal = bParts[i]
		}
		if aVal < bVal {
			return -1
		}
		if aVal > bVal {
			return 1
		}
	}
	return 0
}

// parseVersionParts splits a version string into integer components.
func parseVersionParts(version string) []int {
	// Strip any leading 'v' prefix.
	version = strings.TrimPrefix(strings.TrimSpace(version), "v")

	parts := strings.Split(version, ".")
	result := make([]int, 0, len(parts))
	for _, p := range parts {
		val := 0
		for _, c := range p {
			if c >= '0' && c <= '9' {
				val = val*10 + int(c-'0')
			} else {
				break
			}
		}
		result = append(result, val)
	}
	return result
}
