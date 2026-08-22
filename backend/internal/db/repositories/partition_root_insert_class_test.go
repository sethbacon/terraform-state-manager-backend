package repositories

import (
	"io/fs"
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
		"identity/notify.ChannelRepository.Create, which builds its column list at " +
		"runtime, so no source scan here can read it. It IS stamped: the consumer " +
		"passes notify.WithOwningOrganization from NotificationHandlers.CreateChannel, " +
		"and that side is covered by TestCreateChannel_IsStampedWithTheActingOrganization " +
		"in internal/api. The shared module's own PostgreSQL integration tests cover " +
		"whether the column DEFAULT still fires when the option is omitted -- the half " +
		"a mock cannot see.",
}

var insertPattern = regexp.MustCompile(`(?is)INSERT\s+INTO\s+([a-z_]+)\s*\(([^)]*)\)`)

func TestEveryInsertIntoAPartitionRootNamesTheOrganization(t *testing.T) {
	roots := map[string]bool{}
	for _, r := range tsmPartitionRoots {
		roots[r] = true
	}

	// WALKS THE WHOLE MODULE, not this package.
	//
	// It globbed "*.go" here at first, which covered every root INSERT that exists
	// today -- all eight local ones live in this package. That is exactly the
	// shape of a guard that is BLIND rather than clean: an INSERT written from
	// internal/services or internal/api tomorrow would not be missed by the check,
	// it would be invisible to it, and the check would keep reporting success.
	// The two states are indistinguishable from the outside, so the scan is
	// widened to where the risk actually is.
	//
	// _test.go is still skipped, deliberately: the tenancy integration fixtures
	// INSERT unstamped rows on purpose, because unstamped rows are the condition
	// they exist to reproduce.
	root := filepath.Join("..", "..", "..")
	var files []string
	if err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); name == "vendor" || name == ".git" || name == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go") {
			files = append(files, path)
		}
		return nil
	}); err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}

	seen := map[string]bool{}
	scanned := 0
	for _, f := range files {
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

	// A floor, not just non-zero. "Did it read ANY file" would still pass if the
	// walk collapsed to this one package, which is the narrowing this widening
	// exists to prevent.
	const minFilesForAModuleWideWalk = 100
	if scanned < minFilesForAModuleWideWalk {
		t.Fatalf("scanned only %d source files from %s: a module-wide walk reaches well over that (the module has ~156 today), so "+
			"than that, so this has narrowed back to a single package -- and a blind scan "+
			"looks exactly like a clean one", scanned, root)
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
