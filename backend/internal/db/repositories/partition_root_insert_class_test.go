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

// insertPattern matches an INSERT and captures its target and its column list.
//
// THE TARGET IS OPTIONALLY SCHEMA-QUALIFIED and the column list is OPTIONAL,
// because both of those were once ways to walk straight past this guard. It read
// `INSERT\s+INTO\s+([a-z_]+)\s*\(` at first, which meant:
//
//	INSERT INTO public.state_sources (...)   -- invisible: "public" is the table
//	INSERT INTO state_sources VALUES (...)   -- invisible: no column list to match
//	INSERT INTO %s (...)                     -- invisible: target is not [a-z_]
//	INSERT INTO "public"."state_sources" (.) -- invisible: quotes are not [a-z_]
//
// The fourth was found on 2026-08-27 by an adversarial pass that simply wrote the
// statement a different LEGAL way, and it was not hypothetical: internal/legalhold
// already writes INSERT INTO "public"."legal_holds", the newest table in the tree,
// so the quoted form is the nearest example for whoever adds the tenth root.
//
// The positional one is the dangerous member of that set. A positional INSERT that
// omits organization_id takes the column DEFAULT, which is the exact bug #436
// exists to close, and the guard said nothing about it. None of the three shapes
// is present in the tree today -- which is precisely the point, since a guard
// that only sees the shapes already written is not a guard.
var insertPattern = regexp.MustCompile(`(?is)INSERT\s+INTO\s+((?:"[a-z_]+"|[a-z_]+)(?:\s*\.\s*(?:"[a-z_]+"|[a-z_]+))?|%s)\s*(\(([^)]*)\))?`)

// interpolatedInsertTargets are INSERT sites whose target table is built at
// runtime, so no static scan can know which table they write. Each must be
// justified: an unlisted one fails, because "the guard cannot tell" and "the
// guard is satisfied" must not look the same.
var interpolatedInsertTargets = map[string]string{
	"internal/statesource/pg.go": "the pg state-source CONNECTOR, writing to the customer's own " +
		"Terraform state table in their database -- not to TSM's schema, so it can reach no " +
		"partition root. The identifier is validated in newPG.",
}

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
			target, hadColumnList, columns := m[1], m[2] != "", m[3]

			// A runtime-built target: the scan cannot know the table, so it must be
			// justified rather than skipped.
			if target == "%s" {
				if reason := justifiedInterpolation(f); reason != "" {
					t.Logf("%s: INSERT INTO <interpolated> is justified -- %s", f, reason)
					continue
				}
				t.Errorf("%s: INSERT INTO an interpolated table name. A static scan cannot tell "+
					"which table this writes, so it cannot tell whether a partition root is being "+
					"written unstamped. Add the file to interpolatedInsertTargets with the reason "+
					"it can reach no root.", f)
				continue
			}

			// public.state_sources, "public"."state_sources" and state_sources are
			// all the same table. Quotes are stripped AFTER the split so that a
			// quoted schema cannot hide the dot.
			table := strings.ReplaceAll(target, `"`, "")
			table = strings.TrimSpace(table)
			if i := strings.LastIndex(table, "."); i >= 0 {
				table = strings.TrimSpace(table[i+1:])
			}
			if !roots[table] {
				continue
			}
			seen[table] = true

			// A positional INSERT names no columns, so every column it does not
			// supply takes its DEFAULT -- which for these nine tables is
			// tsm_default_organization_id(). It cannot be audited by reading it,
			// and it is the shape most likely to be wrong.
			if !hadColumnList {
				t.Errorf("%s: INSERT INTO %s with no column list. Every column it omits takes the "+
					"schema DEFAULT, and for a partition root that DEFAULT files the row in ONE "+
					"organization regardless of who is writing. Name the columns.", f, table)
				continue
			}
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
		t.Fatalf("scanned only %d source files from %s: a module-wide walk reaches well over "+
			"that, so this has narrowed back to a single package -- and a blind scan "+
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

// justifiedInterpolation returns the recorded reason an interpolated INSERT
// target is safe, matching on path suffix so the module-root walk's "../../../"
// prefix does not defeat the lookup.
func justifiedInterpolation(path string) string {
	clean := filepath.ToSlash(path)
	for suffix, reason := range interpolatedInsertTargets {
		if strings.HasSuffix(clean, suffix) {
			return reason
		}
	}
	return ""
}

// TestInsertPatternSeesEverySpellingOfATarget pins the SPELLINGS the scan can
// see, because every blind axis this guard has ever had was a legal way of
// writing the same statement that the pattern simply did not match -- and an
// unmatched site is invisible, not reported. Widening the pattern without a case
// here would leave the widening itself unverified.
func TestInsertPatternSeesEverySpellingOfATarget(t *testing.T) {
	cases := []struct {
		name  string
		stmt  string
		table string
	}{
		{"bare", `INSERT INTO state_sources (name) VALUES ($1)`, "state_sources"},
		{"schema qualified", `INSERT INTO public.state_sources (name) VALUES ($1)`, "state_sources"},
		{"quoted table", `INSERT INTO "state_sources" (name) VALUES ($1)`, "state_sources"},
		{"quoted schema and table", `INSERT INTO "public"."state_sources" (name) VALUES ($1)`, "state_sources"},
		{"quoted schema only", `INSERT INTO "public".state_sources (name) VALUES ($1)`, "state_sources"},
		{"lowercase keyword", `insert into "public"."state_sources" (name) VALUES ($1)`, "state_sources"},
		{"no column list", `INSERT INTO "state_sources" VALUES ($1)`, "state_sources"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := insertPattern.FindStringSubmatch(c.stmt)
			if m == nil {
				t.Fatalf("insertPattern did not match %q at all. An unmatched INSERT is "+
					"INVISIBLE to the class guard, which then reports success over a site it "+
					"never read -- the failure mode this whole file exists to prevent.", c.stmt)
			}
			table := strings.ReplaceAll(m[1], `"`, "")
			table = strings.TrimSpace(table)
			if i := strings.LastIndex(table, "."); i >= 0 {
				table = strings.TrimSpace(table[i+1:])
			}
			if table != c.table {
				t.Errorf("resolved target %q, want %q -- the scan would compare the wrong name "+
					"against the partition-root set and skip the site", table, c.table)
			}
		})
	}
}
