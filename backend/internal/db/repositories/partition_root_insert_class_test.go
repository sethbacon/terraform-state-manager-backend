package repositories

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// A CLASS GUARD over the partition roots, rather than one test per root.
//
// #436 stamped nine tables one at a time, and every one of them was found by
// reading code. A tenth arrives the same way -- someone adds a table, writes an
// INSERT, and the row is unowned until a person happens to look. This scans the
// repository layer instead: every INSERT into a partition root must NAME
// organization_id, and a site that must not is listed here with its reason.
//
// It is deliberately a SOURCE scan and not a schema scan. The column exists on
// all nine tables already -- the migration saw to that -- so a schema check is
// green while a statement quietly omits the column and lets the DEFAULT decide.
// The bug lives in the statement text, so that is what this reads.

// tsmPartitionRoots is the same nine tables the tenancy suite enumerates. Kept
// as a literal here because this package cannot import a _test identifier from
// another package; the two lists are cross-checked below by count, so a root
// added there without being added here fails rather than silently narrowing the
// scan.
var tsmPartitionRoots = []string{
	"ci_sources", "drift_records", "drift_runs", "health_runs",
	"notification_channels", "pipeline_connections", "schedules",
	"state_sources", "state_transfers",
}

// exemptInserts are INSERT sites into a root that deliberately do not name
// organization_id, each with the reason it is safe.
//
// EMPTY TODAY, and that is the intended state. drift_records was listed here at
// first on the belief that it derived its owner in-statement without naming the
// column; it does both -- it names organization_id AND fills it from a SELECT
// over state_sources. An exemption that describes a site accurately but is not
// actually needed is worse than none: it stands ready to absorb a real miss on
// that table the day someone changes the statement.
var exemptInserts = map[string]string{}

// externallyOwnedInserts are roots whose INSERT does not live in this repository
// at all. Recorded so that "no site found" is a stated fact rather than the scan
// quietly covering nothing.
var externallyOwnedInserts = map[string]string{
	"notification_channels": "the INSERT lives in terraform-suite-identity's " +
		"identity/notify.ChannelRepository.Create. It is stamped from the consumer side " +
		"via WithOwningOrganization once that module's release lands here.",
}

var insertPattern = regexp.MustCompile(`(?is)INSERT\s+INTO\s+([a-z_]+)\s*\(([^)]*)\)`)

func TestEveryInsertIntoAPartitionRootNamesTheOrganization(t *testing.T) {
	roots := map[string]bool{}
	for _, r := range tsmPartitionRoots {
		roots[r] = true
	}

	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	seen := map[string]bool{}
	scanned := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		scanned++
		for _, m := range insertPattern.FindAllStringSubmatch(string(src), -1) {
			table, columns := m[1], m[2]
			if !roots[table] {
				continue
			}
			seen[table] = true
			if strings.Contains(columns, "organization_id") {
				continue
			}
			if reason, ok := exemptInserts[table]; ok {
				t.Logf("%s: INSERT INTO %s is exempt -- %s", f, table, reason)
				continue
			}
			t.Errorf("%s: INSERT INTO %s does not name organization_id.\n"+
				"A Postgres DEFAULT applies only when a column is OMITTED, so this row is "+
				"being filed wherever the schema's default points -- under 000033 that is "+
				"ONE organization for every tenant. Name the column and bind the acting "+
				"organization, or derive it in the statement and add %s to exemptInserts "+
				"with the reason.\ncolumns: %s", f, table, table, strings.Join(strings.Fields(columns), " "))
		}
	}

	if scanned == 0 {
		t.Fatal("no source files scanned: this guard is looking at the wrong directory, and " +
			"an empty enumeration passes for free -- a blind scan looks exactly like a clean one")
	}

	// Every root must be ACCOUNTED FOR: found here, or recorded as living
	// elsewhere. Silence is the failure mode this catches.
	var unaccounted []string
	for _, root := range tsmPartitionRoots {
		if seen[root] || externallyOwnedInserts[root] != "" {
			continue
		}
		unaccounted = append(unaccounted, root)
	}
	sort.Strings(unaccounted)
	if len(unaccounted) > 0 {
		t.Errorf("no INSERT found for partition root(s) %s, and no entry in "+
			"externallyOwnedInserts explains where they are written. Either the scan stopped "+
			"seeing a statement it used to cover, or a root is written somewhere this guard "+
			"cannot reach -- say which, in externallyOwnedInserts.", strings.Join(unaccounted, ", "))
	}
}

// TestThePartitionRootListMatchesTheTenancySuite keeps the two copies of the
// list from drifting. A root added to the tenancy inventory but not here would
// silently fall outside this scan.
func TestThePartitionRootListMatchesTheTenancySuite(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", "tenancy", "isolation_integration_test.go"))
	if err != nil {
		t.Skipf("tenancy suite not readable from here: %v", err)
	}
	block := regexp.MustCompile(`(?s)var tsmPartitionRoots = \[\]string\{(.*?)\}`).FindStringSubmatch(string(src))
	if block == nil {
		t.Fatal("could not find tsmPartitionRoots in the tenancy suite: this cross-check has " +
			"stopped checking anything")
	}
	theirs := regexp.MustCompile(`"([a-z_]+)"`).FindAllStringSubmatch(block[1], -1)
	var names []string
	for _, m := range theirs {
		names = append(names, m[1])
	}
	sort.Strings(names)
	mine := append([]string(nil), tsmPartitionRoots...)
	sort.Strings(mine)
	if strings.Join(names, ",") != strings.Join(mine, ",") {
		t.Errorf("partition-root lists disagree.\n  tenancy: %s\n  here:    %s",
			strings.Join(names, ", "), strings.Join(mine, ", "))
	}
}
