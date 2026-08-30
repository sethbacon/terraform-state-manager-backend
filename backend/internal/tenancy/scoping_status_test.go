package tenancy

// scoping_status_test.go records, per partition root, whether its reads are
// organization-scoped yet (#502, tracking #393).
//
// WHY THIS EXISTS. The Phase 3 read flip landed for state_sources and shipped
// in v3.13.0; schedules, pipeline_connections and ci_sources followed, and the
// three callback roots after them. Two roots are still unscoped, so this
// application is isolated on seven planes and shared on two -- which is
// neither model, and not a state anyone would choose to sit in indefinitely.
// That is expected DURING a phased migration and dangerous as a resting
// position, and the difference between the two is whether anyone can see it.
//
// Before this, the only way to know which roots were flipped was to read nine
// roots' call sites. An inventory nobody can derive is one that goes stale silently --
// which is exactly how #393's own phase-4 inventory came to name four of five
// tables. This one is derived from PartitionedTables and cannot drift from it:
// adding a root without deciding its scoping status fails, and declaring a
// status for a table that is not a root fails too.

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
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
	// ListSchedules, GetSchedule and RunSchedule all read through
	// ScheduleRepository's ListInScope/GetByIDInScope, and all three routes
	// carry middleware.TenantScope. GetDue stays unscoped BY DESIGN, per the
	// #393 background-authority decision (option B): enumeration is the
	// system's cross-organization job, and every per-item load after it runs
	// under a scope DERIVED from the row it enumerates (tenancy.SystemActingIn).
	"schedules": scopedNow,
	// The background-authority increment (#393 option B): ListPipelines reads
	// ListInScope; the dispatch chain (drift + health + the scheduler) loads
	// connections through GetByIDInScope under a single-organization authority;
	// every /pipelines route carries middleware.TenantScope.
	"pipeline_connections": scopedNow,
	// Same increment. Every by-id read -- discovery, verify, the repo-setup
	// wizard, and resolvePipelineToken's shared-credential hop, previously
	// entirely unscoped -- goes through GetByIDInScope; all /ci-sources routes
	// carry middleware.TenantScope.
	"ci_sources": scopedNow,

	// The callback roots (#393 option B, item 5). These are the three with TWO
	// kinds of reader, and both had to close before either could be called done:
	// a person reads them through a request that resolves a scope
	// (ListInScope/GetByIDInScope behind middleware.TenantScope on every
	// /drift and /health-lab read route, plus the acknowledge and resolve
	// writes), and a CI job posts results to them holding a per-run bearer token
	// and no principal at all. The machine path derives its authority FROM THE
	// CREDENTIAL -- the run the token authenticates names the organization -- so
	// its callback route carries no TenantScope by design and is not a gap.
	// See internal/api/callback_authority.go.
	//
	// The one read that stays unscoped on each is the pre-authentication lookup
	// the token is compared against; it is recorded per-method in
	// unscoped_twin_class_test.go's justifiedUnscoped.
	"drift_runs":    scopedNow,
	"drift_records": scopedNow,
	"health_runs":   scopedNow,

	"notification_channels": unscopedPending,
	"state_transfers":       unscopedPending,
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

// TestTheOperatorFacingTableMatchesThisDeclaration closes the second hand-copy.
//
// docs/tenancy-decision.md is the page an operator reads to find out whether
// this deployment is isolated, and it carried its own two-column table of which
// roots are scoped. It also asserted, in the same paragraph, that the table was
// "not maintained by hand" -- which it was. The schedules flip found it still
// naming one scoped root.
//
// An inventory duplicated is an inventory that will disagree, and the copy a
// reader trusts is whichever one they opened. So the page is parsed here and
// compared to rootScoping IN BOTH DIRECTIONS: a flip that updates the code and
// forgets the page fails, and so does a page edited ahead of the code.
//
// The doc is located relative to this package rather than by walking upward for
// a marker: a search that failed to find it would have to either fail the build
// on a legitimate layout change or skip, and a skip here is a guard that reports
// nothing while looking exactly like one that checked.
func TestTheOperatorFacingTableMatchesThisDeclaration(t *testing.T) {
	const docPath = "../../../docs/tenancy-decision.md"
	raw, err := os.ReadFile(docPath)
	if err != nil {
		t.Fatalf("read %s: %v.\nThis guard cannot check a page it cannot open. If the file moved, "+
			"point this at it -- do not delete the check, because the page is what an operator "+
			"reads to decide whether their deployment is isolated.", docPath, err)
	}

	scoped, pending, err := parseScopingTable(string(raw))
	if err != nil {
		t.Fatalf("%s: %v", docPath, err)
	}

	// A floor before comparing. An unparsed table yields two empty sets, and two
	// empty sets would compare equal to each other while telling us nothing.
	if len(scoped)+len(pending) != len(rootScoping) {
		t.Fatalf("%s lists %d roots across both columns; rootScoping declares %d. "+
			"scoped=%v pending=%v", docPath, len(scoped)+len(pending), len(rootScoping), scoped, pending)
	}

	for _, tbl := range scoped {
		st, ok := rootScoping[tbl]
		if !ok {
			t.Errorf("%s calls %q organization-scoped, but it is not a declared partition root.", docPath, tbl)
			continue
		}
		if st != scopedNow {
			t.Errorf("%s says %q has organization-scoped reads; rootScoping says it does not. "+
				"The page is what an operator reads to decide whether their deployment is isolated, "+
				"so this direction is the dangerous one.", docPath, tbl)
		}
	}
	for _, tbl := range pending {
		st, ok := rootScoping[tbl]
		if !ok {
			t.Errorf("%s lists %q as unscoped, but it is not a declared partition root.", docPath, tbl)
			continue
		}
		if st != unscopedPending {
			t.Errorf("%s still lists %q as returning every row; rootScoping says its reads are "+
				"scoped. Update the page in the change that flips the root, not afterwards.", docPath, tbl)
		}
	}
}

// parseScopingTable pulls the two cells out of the single data row of the
// "Reads are organization-scoped" table and returns the backticked identifiers
// in each.
//
// It returns an error rather than empty slices when it cannot find the table, so
// a page that was restructured fails loudly instead of passing with nothing
// enumerated.
func parseScopingTable(doc string) (scoped, pending []string, err error) {
	lines := strings.Split(doc, "\n")
	header := -1
	for i, ln := range lines {
		if strings.Contains(ln, "Reads are organization-scoped") && strings.Contains(ln, "|") {
			header = i
			break
		}
	}
	if header < 0 {
		return nil, nil, errors.New("no table header containing \"Reads are organization-scoped\"; " +
			"the page was restructured and this guard is no longer reading the inventory")
	}
	// header, then the alignment row, then the single data row.
	if header+2 >= len(lines) {
		return nil, nil, errors.New("the scoping table has a header but no data row")
	}
	cells := strings.Split(strings.Trim(strings.TrimSpace(lines[header+2]), "|"), "|")
	if len(cells) != 2 {
		return nil, nil, fmt.Errorf("the scoping table's data row has %d cells, want 2: %q",
			len(cells), lines[header+2])
	}
	return backticked(cells[0]), backticked(cells[1]), nil
}

// backticked returns the `identifiers` in one markdown table cell.
func backticked(cell string) []string {
	var out []string
	for _, part := range strings.Split(cell, "`") {
		part = strings.TrimSpace(part)
		// Splitting on a backtick alternates outside/inside; the separators
		// between entries are ", " and trim to empty, so only real identifiers
		// survive this filter.
		if part == "" || strings.HasPrefix(part, ",") {
			continue
		}
		out = append(out, part)
	}
	sort.Strings(out)
	return out
}
