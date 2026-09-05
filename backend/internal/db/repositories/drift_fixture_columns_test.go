package repositories

import (
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/terraform-state-manager/terraform-state-manager/internal/testsupport"
)

// This file is the guard the code-review that landed testsupport asked for:
// a test that fails when driftColumns gains (or loses, or reorders) a column
// and testsupport.DriftRunColumns does not follow, so the three packages that
// build a drift_runs sqlmock fixture (this one, internal/services/
// driftreconcile, internal/api) cannot silently drift from scanDrift's real
// column order. Only this package can see the unexported driftColumns const,
// which is why the guard lives here rather than in testsupport itself.

// driftColumnFragmentRe recognizes the two SQL shapes driftColumns wraps a
// bare column name in: `COALESCE(name,...)` and `name::cast`. Anything else
// is asserted to already be a bare identifier; a shape this cannot parse
// fails loudly rather than silently comparing the wrong string.
var (
	driftColumnCoalesceRe = regexp.MustCompile(`^COALESCE\(([a-zA-Z_][a-zA-Z0-9_]*)\s*,`)
	driftColumnIdentRe    = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)
)

// splitTopLevelCommas splits s on commas that are NOT inside parentheses, so
// `COALESCE(state_key,”)` survives as one fragment instead of being cut at
// its internal comma.
func splitTopLevelCommas(s string) []string {
	var out []string
	depth := 0
	start := 0
	for i, r := range s {
		switch r {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, s[start:i])
				start = i + 1
			}
		}
	}
	out = append(out, s[start:])
	return out
}

// bareDriftColumnName extracts the underlying column name from one
// driftColumns fragment (already whitespace-trimmed by the caller).
func bareDriftColumnName(t *testing.T, expr string) string {
	t.Helper()
	if m := driftColumnCoalesceRe.FindStringSubmatch(expr); m != nil {
		return m[1]
	}
	if idx := strings.Index(expr, "::"); idx != -1 {
		name := expr[:idx]
		if !driftColumnIdentRe.MatchString(name) {
			t.Fatalf("driftColumns fragment %q casts something that is not a bare identifier", expr)
		}
		return name
	}
	if !driftColumnIdentRe.MatchString(expr) {
		t.Fatalf("driftColumns fragment %q is not a COALESCE(...), a ::cast, or a bare identifier -- "+
			"teach bareDriftColumnName this shape rather than silently comparing the wrong string", expr)
	}
	return expr
}

// TestDriftRunColumns_MatchesProductionDriftColumns is the guard: it parses
// the ACTUAL driftColumns SQL constant scanDrift selects (not a copy of it)
// down to bare column names and asserts the result equals
// testsupport.DriftRunColumns, in order. A column added to driftColumns
// without updating testsupport.DriftRunColumns fails this test by name,
// rather than as a confusing "wrong number of Scan destinations" panic three
// packages away.
func TestDriftRunColumns_MatchesProductionDriftColumns(t *testing.T) {
	var got []string
	for _, frag := range splitTopLevelCommas(driftColumns) {
		got = append(got, bareDriftColumnName(t, strings.TrimSpace(frag)))
	}
	if !reflect.DeepEqual(got, testsupport.DriftRunColumns) {
		t.Fatalf("testsupport.DriftRunColumns has drifted from the production driftColumns const.\n"+
			"  parsed from driftColumns:    %v\n"+
			"  testsupport.DriftRunColumns: %v\n"+
			"update testsupport.DriftRunColumns (and gofmt/gate every fixture built on it) to match "+
			"scanDrift's actual column order.", got, testsupport.DriftRunColumns)
	}
}
