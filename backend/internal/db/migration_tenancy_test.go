package db

import (
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/terraform-state-manager/terraform-state-manager/internal/tenancy"
)

// The class this file guards: THE ORGANIZATION PARTITION DISAGREEING WITH ITSELF.
//
// #393 phase 1 states one fact in three places — migration 000033's ADD COLUMNs,
// the same migration's SET DEFAULTs, and tenancy.PartitionedTables, which is what
// the startup backfill iterates. Each is edited by hand, and every pairwise
// disagreement between them is silent at the moment it is introduced and
// expensive later:
//
//   - A table in the migration but not in PartitionedTables gets the column and
//     the default, so NEW rows are stamped and every row that predates the
//     migration keeps organization_id NULL forever. Nothing notices until phase 4
//     runs SET NOT NULL and fails on a production table.
//   - A table in PartitionedTables but not in the migration makes the backfill
//     UPDATE a column that does not exist. bootstrap.Run returns the error and the
//     server does not start.
//   - A table given the column but not the DEFAULT silently writes NULL on every
//     INSERT from that point on. This is the worst of the three: it looks
//     completely correct, the backfill even cleans it up on the next boot, and the
//     window between them is invisible.
//   - A column added by the up migration and not dropped by the down migration
//     makes the rollback a partial one, and the re-apply then meets a column that
//     already exists.
//
// All four are pure functions of the checked-in tree, so they are answerable
// here, on the branch that introduces them, with no database.

// migration000033 is the file under guard. Named as a constant so a rename of
// the migration fails these tests loudly rather than leaving them reading
// nothing — see readMigrationStatements's empty check.
const migration000033 = "000033_organization_partition"

var (
	// Anchored to the start of a statement so a table name occurring inside a
	// prose paragraph cannot be mistaken for a declaration. Comment lines are
	// stripped before matching in any case; this is the second half of that belt.
	reAddOrgColumn  = regexp.MustCompile(`(?m)^\s*ALTER TABLE\s+(\w+)\s+ADD COLUMN IF NOT EXISTS\s+organization_id\s+UUID\s*;`)
	reSetOrgDefault = regexp.MustCompile(
		`(?m)^\s*ALTER TABLE\s+(\w+)\s+ALTER COLUMN\s+organization_id\s+SET DEFAULT\s+tsm_default_organization_id\(\)\s*;`)
	reDropOrgColumn = regexp.MustCompile(`(?m)^\s*ALTER TABLE\s+(\w+)\s+DROP COLUMN IF EXISTS\s+organization_id\s*;`)
)

// readMigration returns one migration's SQL with comment lines removed.
//
// From the embedded FS, for migrationFiles' reason: a guard reading the working
// directory would certify text the binary never sees. Comments are stripped
// because 000033's header discusses these statements at length and a guard that
// matched prose would pass on a migration whose SQL had been deleted entirely.
func readMigrationStatements(t *testing.T, name string) string {
	t.Helper()
	raw, err := migrationsFS.ReadFile("migrations/" + name + ".sql")
	if err != nil {
		t.Fatalf("reading migrations/%s.sql from the embedded FS: %v.\n"+
			"If this migration was renamed, rename it here too — a guard that cannot "+
			"find its subject must fail, not pass.", name, err)
	}
	var kept []string
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "--") {
			continue
		}
		kept = append(kept, line)
	}
	sql := strings.Join(kept, "\n")
	if strings.TrimSpace(sql) == "" {
		t.Fatalf("migrations/%s.sql contains no statements once comments are stripped — "+
			"the guard is inspecting an empty universe", name)
	}
	return sql
}

// tablesMatching returns the sorted, de-duplicated capture group 1 of every match.
func orgTablesMatching(re *regexp.Regexp, sql string) []string {
	seen := map[string]bool{}
	for _, m := range re.FindAllStringSubmatch(sql, -1) {
		seen[m[1]] = true
	}
	out := make([]string, 0, len(seen))
	for t := range seen {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

// diff reports what is in a and not b, and in b and not a. Both directions,
// always: a guard that only checked one would certify the half of the drift that
// happened to be tested and stay green on the other.
func symmetricDiff(a, b []string) (onlyA, onlyB []string) {
	inB := map[string]bool{}
	for _, s := range b {
		inB[s] = true
	}
	inA := map[string]bool{}
	for _, s := range a {
		inA[s] = true
	}
	for _, s := range a {
		if !inB[s] {
			onlyA = append(onlyA, s)
		}
	}
	for _, s := range b {
		if !inA[s] {
			onlyB = append(onlyB, s)
		}
	}
	return onlyA, onlyB
}

// TestPartitionedTablesMatchTheMigration pins tenancy.PartitionedTables — the
// list the startup backfill sweeps — to the tables 000033 actually gave a column.
func TestPartitionedTablesMatchTheMigration(t *testing.T) {
	inMigration := orgTablesMatching(reAddOrgColumn, readMigrationStatements(t, migration000033+".up"))
	if len(inMigration) == 0 {
		t.Fatal("000033 declares no `ADD COLUMN IF NOT EXISTS organization_id UUID` at all — " +
			"either the migration lost its statements or this guard's pattern stopped matching them. " +
			"Both are failures; neither is a pass.")
	}
	inGo := sortedCopy(tenancy.PartitionedTables)

	missingFromGo, missingFromSQL := symmetricDiff(inMigration, inGo)
	if len(missingFromGo) > 0 {
		t.Errorf("000033 adds organization_id to %v, which tenancy.PartitionedTables does not list.\n"+
			"The column default stamps NEW rows, so this looks correct — but nothing backfills the rows that "+
			"predate the migration, and their organization_id stays NULL until phase 4's SET NOT NULL fails on "+
			"a production table.", missingFromGo)
	}
	if len(missingFromSQL) > 0 {
		t.Errorf("tenancy.PartitionedTables lists %v, which 000033 does not give an organization_id.\n"+
			"The backfill will UPDATE a column that does not exist, bootstrap.Run will return the error, and "+
			"the server will not start.", missingFromSQL)
	}
}

// TestEveryPartitionedColumnHasItsDefault is the guard for the quietest of the
// four failures.
//
// ADD COLUMN and SET DEFAULT are separate statements in 000033 on purpose — a
// single `ADD COLUMN ... DEFAULT <function call>` rewrites the whole table under
// ACCESS EXCLUSIVE, which on a large drift_runs is an outage. The cost of
// splitting them is that one can be written without the other, and the result is
// a table whose every INSERT writes NULL while looking entirely correct.
func TestEveryPartitionedColumnHasItsDefault(t *testing.T) {
	up := readMigrationStatements(t, migration000033+".up")
	withColumn := orgTablesMatching(reAddOrgColumn, up)
	withDefault := orgTablesMatching(reSetOrgDefault, up)
	if len(withColumn) == 0 {
		t.Fatal("no organization_id columns found in 000033 — the guard is vacuous")
	}

	noDefault, noColumn := symmetricDiff(withColumn, withDefault)
	if len(noDefault) > 0 {
		t.Errorf("%v are given organization_id but no DEFAULT tsm_default_organization_id().\n"+
			"Every INSERT into them writes NULL, and phase 4 cannot make the column NOT NULL. A default is the "+
			"only mechanism that covers write paths nobody remembered to edit — which is the entire reason "+
			"phase 1 uses one instead of touching nine repositories.", noDefault)
	}
	if len(noColumn) > 0 {
		t.Errorf("%v are given a DEFAULT for organization_id but no such column. This migration cannot apply.", noColumn)
	}
}

// TestTheDownMigrationDropsEveryColumnTheUpAdds keeps the rollback whole.
//
// TestEveryMigrationHasBothDirections proves a down FILE exists; it says nothing
// about whether that file reverses anything. A down leg that dropped eight of
// nine columns would leave the ninth behind, and the re-apply would then meet a
// column that already exists — the IF NOT EXISTS makes that silent, so the
// schema would carry a column no migration believes it created.
func TestTheDownMigrationDropsEveryColumnTheUpAdds(t *testing.T) {
	added := orgTablesMatching(reAddOrgColumn, readMigrationStatements(t, migration000033+".up"))
	dropped := orgTablesMatching(reDropOrgColumn, readMigrationStatements(t, migration000033+".down"))
	if len(added) == 0 || len(dropped) == 0 {
		t.Fatalf("nothing to compare (up added %d, down dropped %d) — the guard is vacuous", len(added), len(dropped))
	}

	notDropped, notAdded := symmetricDiff(added, dropped)
	if len(notDropped) > 0 {
		t.Errorf("000033's up adds organization_id to %v and its down does not drop it: the rollback is partial", notDropped)
	}
	if len(notAdded) > 0 {
		t.Errorf("000033's down drops organization_id from %v, which its up never added", notAdded)
	}
}
