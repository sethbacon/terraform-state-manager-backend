package analyzer

import (
	"context"
	"strings"
)

// Provider pin-drift status values for a ProviderPinDriftReport.
const (
	// ProviderPinUpToDate means the locked provider version equals the latest
	// non-prerelease version published to the registry.
	ProviderPinUpToDate = "up_to_date"
	// ProviderPinBehind means a newer version is available than the locked pin.
	ProviderPinBehind = "behind"
	// ProviderPinUnknown means drift could not be determined — the latest
	// version is missing/unresolvable, or the pin/version is unparseable.
	ProviderPinUnknown = "unknown"
)

// ProviderPinDriftReport is the per-provider analogue of VersionDriftReport: it
// compares a provider's locked version (from `.terraform.lock.hcl`) against the
// latest version available from the Terraform Registry, and reports whether the
// locked version still satisfies the recorded version constraint.
type ProviderPinDriftReport struct {
	// Source is the fully-qualified provider source address, e.g.
	// "registry.terraform.io/hashicorp/aws".
	Source string `json:"source"`
	// Pinned is the exact locked version from the lock file, e.g. "5.31.0".
	Pinned string `json:"pinned"`
	// LatestAvailable is the newest non-prerelease version from the registry.
	// Empty when it could not be resolved.
	LatestAvailable string `json:"latest_available"`
	// Constraint is the recorded version constraint (the required_providers
	// constraint resolved into the lock file), e.g. ">= 5.0.0, < 6.0.0". May be
	// empty when the lock file recorded no constraint.
	Constraint string `json:"constraint,omitempty"`
	// SatisfiesConstraint is true when the pinned version meets Constraint. It is
	// false when no constraint is recorded or the constraint is unparseable.
	SatisfiesConstraint bool `json:"satisfies_constraint"`
	// Status is one of ProviderPinUpToDate, ProviderPinBehind, ProviderPinUnknown.
	Status string `json:"status"`
}

// ComputeProviderPinDrift evaluates each provider lock pin against the latest
// version reported by the given ProviderVersionSource, producing one drift
// report per pin in the same order as the input.
//
// The function never fails the analysis: a pin whose latest version cannot be
// resolved (unknown provider, network failure, unparseable address or versions)
// is reported with status "unknown" rather than aborting the batch. When source
// is nil, every pin is reported as "unknown".
//
// SatisfiesConstraint is computed independently of registry availability, using
// the constraint recorded in the lock pin (the resolved required_providers
// constraint) and the package's existing constraint evaluator.
func ComputeProviderPinDrift(ctx context.Context, pins []ProviderLockPin, source ProviderVersionSource) []ProviderPinDriftReport {
	reports := make([]ProviderPinDriftReport, 0, len(pins))

	for _, pin := range pins {
		report := ProviderPinDriftReport{
			Source:     pin.Source,
			Pinned:     strings.TrimSpace(pin.Version),
			Constraint: strings.TrimSpace(pin.Constraints),
			Status:     ProviderPinUnknown,
		}

		// Constraint satisfaction is independent of the registry lookup.
		if report.Constraint != "" && report.Pinned != "" {
			if satisfies, ok := constraintSatisfied(report.Constraint, report.Pinned); ok {
				report.SatisfiesConstraint = satisfies
			}
		}

		report.LatestAvailable, report.Status = resolvePinStatus(ctx, pin, source)
		reports = append(reports, report)
	}

	return reports
}

// resolvePinStatus determines the latest available version for a pin and the
// resulting up_to_date/behind/unknown status. It isolates all the "unknown"
// branches (no source, unparseable address, lookup failure, no stable version,
// unparseable pin) so ComputeProviderPinDrift stays linear.
func resolvePinStatus(ctx context.Context, pin ProviderLockPin, source ProviderVersionSource) (latest, status string) {
	if source == nil {
		return "", ProviderPinUnknown
	}

	namespace, providerType, ok := parseProviderSource(pin.Source)
	if !ok {
		return "", ProviderPinUnknown
	}

	latest, err := source.LatestVersion(ctx, namespace, providerType)
	if err != nil || latest == "" {
		return "", ProviderPinUnknown
	}

	pinned := strings.TrimSpace(pin.Version)
	if pinned == "" || !isNumericVersion(pinned) || !isNumericVersion(latest) {
		return latest, ProviderPinUnknown
	}

	if compareVersions(pinned, latest) >= 0 {
		return latest, ProviderPinUpToDate
	}
	return latest, ProviderPinBehind
}
