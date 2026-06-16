// Package version provides a small, spec-correct semantic-version comparator
// built on golang.org/x/mod/semver. It backs the module-freshness check, which
// decides whether the module version locked in a state is behind the latest
// version a registry publishes. Registry module versions are published without
// the leading "v" (e.g. "5.3.0"); x/mod/semver requires it, so we normalize.
package version

import "golang.org/x/mod/semver"

// normalize prepends the "v" prefix x/mod/semver requires and reports whether
// the result is a valid semantic version.
func normalize(v string) (string, bool) {
	if v == "" {
		return "", false
	}
	if v[0] != 'v' {
		v = "v" + v
	}
	if !semver.IsValid(v) {
		return "", false
	}
	return v, true
}

// IsValid reports whether v is a comparable semantic version (with or without a
// leading "v").
func IsValid(v string) bool {
	_, ok := normalize(v)
	return ok
}

// Compare returns -1, 0, or +1 as a is less than, equal to, or greater than b,
// by semantic-version precedence: pre-releases sort below their release
// (1.2.0-rc1 < 1.2.0) and build metadata (+meta) is ignored. Invalid inputs sort
// lowest — an invalid value is less than any valid one, two invalids are equal —
// so callers should skip invalid registry tags from "latest" selection rather
// than rely on this ordering.
func Compare(a, b string) int {
	na, aok := normalize(a)
	nb, bok := normalize(b)
	switch {
	case !aok && !bok:
		return 0
	case !aok:
		return -1
	case !bok:
		return 1
	default:
		return semver.Compare(na, nb)
	}
}
