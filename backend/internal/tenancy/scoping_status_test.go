package tenancy

// scoping_status_test.go records, per partition root, whether its reads are
// organization-scoped yet (#502, tracking #393).
//
// WHY THIS EXISTS. The Phase 3 read flip landed for state_sources and shipped
// in v3.13.0. The other eight roots are still unscoped, so this application is
// isolated on one plane and shared on eight -- which is neither model, and not
// a state anyone would choose to sit in indefinitely. That is expected DURING a
// phased migration and dangerous as a resting position, and the difference
// between the two is whether anyone can see it.
//
// Before this, the only way to know which roots were flipped was to read nine
// call sites. An inventory nobody can derive is one that goes stale silently --
// which is exactly how #393's own phase-4 inventory came to name four of five
// tables. This one is derived from PartitionedTables and cannot drift from it:
// adding a root without deciding its scoping status fails, and declaring a
// status for a table that is not a root fails too.

import (
	"sort"
	"testing"
)

// scopingStatus is where each partition root's reads stand.
type scopingStatus int

const (
	// scopedNow means reads go through a tenant-scoped reader with no
	// unscoped fallback. A member of one organization cannot see another's.
	scopedNow scopingStatus = iota
	// unscopedPending means reads still return every row regardless of the
	// caller's organization. Tracked by #393, not accepted as final.
	unscopedPending
)

// rootScoping is the declared state of each partition root.
//
// Move an entry to scopedNow in the SAME change that flips its reads, never
// before: this table is read by people deciding whether the migration is done.
var rootScoping = map[string]scopingStatus{
	"state_sources": scopedNow, // Phase 3 flip, eba27bb, shipped v3.13.0

	"pipeline_connections":  unscopedPending,
	"ci_sources":            unscopedPending,
	"notification_channels": unscopedPending,
	"schedules":             unscopedPending,
	"state_transfers":       unscopedPending,
	"drift_runs":            unscopedPending,
	"drift_records":         unscopedPending,
	"health_runs":           unscopedPending,
}

// TestEveryPartitionRootHasADeclaredScopingStatus checks the inventory against
// PartitionedTables in both directions.
func TestEveryPartitionRootHasADeclaredScopingStatus(t *testing.T) {
	if len(PartitionedTables) == 0 {
		t.Fatal("PartitionedTables is empty; every assertion below would pass while checking nothing")
	}

	roots := map[string]bool{}
	for _, tbl := range PartitionedTables {
		roots[tbl] = true
		if _, ok := rootScoping[tbl]; !ok {
			t.Errorf("partition root %q has no declared scoping status.\n"+
				"Adding a root is a decision about whether its reads are organization-scoped. "+
				"Record it in rootScoping -- unscopedPending is a fine answer, silence is not (#502).", tbl)
		}
	}
	for tbl := range rootScoping {
		if !roots[tbl] {
			t.Errorf("rootScoping declares %q, which is not in PartitionedTables. "+
				"Renamed, or is the root gone?", tbl)
		}
	}
}

// TestScopingProgressIsVisible fails nothing on its own and prints the state of
// the migration, so a reader of a CI log does not have to open nine files.
//
// Deliberately NOT an assertion that all roots are scoped: that would fail every
// build for the whole duration of a phased migration, and a permanently-red
// check is as uninformative as a permanently-green one.
func TestScopingProgressIsVisible(t *testing.T) {
	var scoped, pending []string
	for tbl, st := range rootScoping {
		if st == scopedNow {
			scoped = append(scoped, tbl)
		} else {
			pending = append(pending, tbl)
		}
	}
	sort.Strings(scoped)
	sort.Strings(pending)

	t.Logf("organization-scoped reads (%d/%d): %v", len(scoped), len(rootScoping), scoped)
	t.Logf("still unscoped, tracked by #393 (%d/%d): %v", len(pending), len(rootScoping), pending)

	if len(scoped) == 0 {
		t.Error("no partition root is declared scoped. The Phase 3 flip for state_sources shipped " +
			"in v3.13.0, so either this table has lost touch with the code or that flip was reverted " +
			"without anyone updating it.")
	}
}
