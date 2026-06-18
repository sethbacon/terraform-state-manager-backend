package api

import (
	"testing"

	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
)

// rows mirrors a small spread of analyzed states across versions, including the
// "" (unknown) bucket and a non-semver value, to exercise the drill-down filter.
func versionFilterRows() []repositories.StateVersionRow {
	return []repositories.StateVersionRow{
		{SourceID: "s1", SourceName: "prod", StateKey: "a.tfstate", TerraformVersion: "0.12.31"},
		{SourceID: "s1", SourceName: "prod", StateKey: "b.tfstate", TerraformVersion: "0.14.11"},
		{SourceID: "s1", SourceName: "prod", StateKey: "c.tfstate", TerraformVersion: "1.0.0"},
		{SourceID: "s2", SourceName: "dev", StateKey: "d.tfstate", TerraformVersion: "1.5.7"},
		{SourceID: "s2", SourceName: "dev", StateKey: "e.tfstate", TerraformVersion: "1.5.7"},
		{SourceID: "s2", SourceName: "dev", StateKey: "f.tfstate", TerraformVersion: ""},
		{SourceID: "s3", SourceName: "legacy", StateKey: "g.tfstate", TerraformVersion: "not-a-version"},
	}
}

func keysOf(rows []repositories.StateVersionRow) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.StateKey)
	}
	return out
}

func equalKeys(got []repositories.StateVersionRow, want []string) bool {
	keys := keysOf(got)
	if len(keys) != len(want) {
		return false
	}
	for i := range want {
		if keys[i] != want[i] {
			return false
		}
	}
	return true
}

func TestFilterStatesByVersion(t *testing.T) {
	rows := versionFilterRows()

	cases := []struct {
		name string
		ver  string
		op   string
		want []string
	}{
		{"eq exact", "1.5.7", "eq", []string{"d.tfstate", "e.tfstate"}},
		{"eq single", "1.0.0", "eq", []string{"c.tfstate"}},
		{"eq unknown maps to empty version", "unknown", "eq", []string{"f.tfstate"}},
		{"eq no match", "9.9.9", "eq", nil},
		// Everything older than 1.0.0 — the headline use case. Skips the empty
		// and non-semver rows.
		{"lt 1.0.0", "1.0.0", "lt", []string{"a.tfstate", "b.tfstate"}},
		{"lte 1.0.0 includes the boundary", "1.0.0", "lte", []string{"a.tfstate", "b.tfstate", "c.tfstate"}},
		{"gt 1.0.0", "1.0.0", "gt", []string{"d.tfstate", "e.tfstate"}},
		{"gte 1.0.0 includes the boundary", "1.0.0", "gte", []string{"c.tfstate", "d.tfstate", "e.tfstate"}},
		// A non-semver pivot can't anchor a range, so nothing matches.
		{"range with non-semver pivot", "unknown", "lt", nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := filterStatesByVersion(rows, tc.ver, tc.op)
			if !equalKeys(got, tc.want) {
				t.Errorf("filterStatesByVersion(%q, %q) = %v, want %v", tc.ver, tc.op, keysOf(got), tc.want)
			}
		})
	}
}

func TestFilterStatesByVersion_RangeSkipsNonSemver(t *testing.T) {
	// The "" (unknown) and "not-a-version" rows must never appear in a range
	// result, even one broad enough to otherwise include everything.
	got := filterStatesByVersion(versionFilterRows(), "0.0.1", "gte")
	for _, r := range got {
		if r.TerraformVersion == "" || r.TerraformVersion == "not-a-version" {
			t.Errorf("range result unexpectedly included non-semver version %q (%s)", r.TerraformVersion, r.StateKey)
		}
	}
}
