package db

import (
	"io/fs"
	"path"
	"sort"
	"strings"
	"testing"
)

// The class this file guards: TWO MIGRATIONS CLAIMING THE SAME VERSION.
//
// This is not hypothetical. Two pull requests merged within a day of each other
// each added a `000031_*`: #388's drift_runs_completeness and #387's
// app_role_authorization. Neither branch could see the other's number, both CI
// runs were green, and the result on main was a server that could not START —
// golang-migrate's iofs source refuses to initialise on a duplicate version, so
// `RunMigrations` failed before any statement ran, and with it every
// PostgreSQL-backed test in the tree.
//
// Nothing caught it because nothing looked. Every existing migration test asserts
// on the CONTENT of a particular file; none asserted on the SET. So the failure
// surfaced as "the integration suite cannot create its database", three commits
// downstream, in a change that had nothing to do with either migration.
//
// A guard on the set is cheap, is a pure function of the checked-in tree, and
// fails in `go test ./internal/db` on the branch that introduces the collision —
// which is the only moment at which the number is still free to change.

// migrationFile is one parsed migration filename.
type migrationFile struct {
	name      string
	version   string
	direction string
}

// migrationFiles parses every embedded migration filename.
//
// Read from the SAME embed.FS the binary runs from, not from the directory: a
// guard reading the directory would certify files the binary never sees, and the
// //go:embed pattern is exactly the thing that decides which those are.
func migrationFiles(t *testing.T) []migrationFile {
	t.Helper()
	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		t.Fatalf("reading the embedded migrations: %v", err)
	}
	var out []migrationFile
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		name := e.Name()
		version, _, ok := strings.Cut(name, "_")
		if !ok {
			t.Fatalf("migration %q does not start with a version prefix", name)
		}
		base := strings.TrimSuffix(name, path.Ext(name))
		direction := base[strings.LastIndex(base, ".")+1:]
		out = append(out, migrationFile{name: name, version: version, direction: direction})
	}
	if len(out) == 0 {
		t.Fatal("no migrations were embedded: the guard inspected an empty universe " +
			"(the //go:embed pattern stopped matching, which is itself a boot failure)")
	}
	return out
}

// TestNoTwoMigrationsShareAVersion is the guard the 000031 collision needed.
//
// golang-migrate keys the schema_migrations table on the numeric version, so two
// files claiming one number is not a naming preference — it is an ambiguity the
// source driver refuses to resolve, and it takes the whole binary down with it.
func TestNoTwoMigrationsShareAVersion(t *testing.T) {
	byVersion := map[string]map[string][]string{}
	for _, f := range migrationFiles(t) {
		if byVersion[f.version] == nil {
			byVersion[f.version] = map[string][]string{}
		}
		byVersion[f.version][f.direction] = append(byVersion[f.version][f.direction], f.name)
	}

	var collisions []string
	for version, directions := range byVersion {
		for direction, names := range directions {
			if len(names) > 1 {
				sort.Strings(names)
				collisions = append(collisions, version+"."+direction+": "+strings.Join(names, ", "))
			}
		}
	}
	if len(collisions) > 0 {
		sort.Strings(collisions)
		t.Fatalf("these migration versions are claimed by more than one file: %v.\n"+
			"golang-migrate's iofs source refuses to initialise on a duplicate version, so this is not a naming "+
			"nit: RunMigrations fails before any statement runs and the server cannot start. Renumber the one that "+
			"has NOT been applied anywhere. Two pull requests each taking the next free number is how this happens, "+
			"and neither one's CI can see the other — which is why the check lives on the merged set.", collisions)
	}
}

// TestEveryMigrationHasBothDirections keeps the down leg from going missing.
//
// A rollback nobody can run is not a rollback plan, and an up-only migration is
// invisible until the moment somebody needs to reverse it. Cheap to assert here,
// and it shares the collision guard's parse so a filename convention change
// cannot silently disable one without the other.
func TestEveryMigrationHasBothDirections(t *testing.T) {
	seen := map[string]map[string]bool{}
	for _, f := range migrationFiles(t) {
		if seen[f.version] == nil {
			seen[f.version] = map[string]bool{}
		}
		seen[f.version][f.direction] = true
	}
	var incomplete []string
	for version, directions := range seen {
		if !directions["up"] || !directions["down"] {
			incomplete = append(incomplete, version)
		}
	}
	if len(incomplete) > 0 {
		sort.Strings(incomplete)
		t.Fatalf("these migration versions do not have both an .up.sql and a .down.sql: %v", incomplete)
	}
}

// TestMigrationVersionsAreContiguous catches the OTHER outcome of two branches
// picking numbers independently: a gap, left behind when a collision is resolved
// by renumbering and the abandoned number is never reused.
//
// A gap is harmless to golang-migrate, which orders by version and does not
// require density. It is not harmless to a reviewer, for whom "000033 is next"
// stops being derivable from the highest number present — which is precisely the
// reasoning that produced the collision in the first place.
func TestMigrationVersionsAreContiguous(t *testing.T) {
	versions := map[string]bool{}
	for _, f := range migrationFiles(t) {
		versions[f.version] = true
	}
	ordered := make([]string, 0, len(versions))
	for v := range versions {
		ordered = append(ordered, v)
	}
	sort.Strings(ordered)

	var gaps []string
	for i := 1; i < len(ordered); i++ {
		prev, cur := ordered[i-1], ordered[i]
		if !isSuccessor(prev, cur) {
			gaps = append(gaps, prev+" -> "+cur)
		}
	}
	if len(gaps) > 0 {
		t.Fatalf("the migration numbering is not contiguous: %v.\n"+
			"golang-migrate does not mind, but the next free number stops being derivable from the highest one "+
			"present — which is the assumption two branches each made when they both took 000031.", gaps)
	}
}

// isSuccessor reports whether cur is prev+1, comparing as fixed-width decimal
// strings so the comparison does not depend on a width the filenames choose.
func isSuccessor(prev, cur string) bool {
	p, pok := versionNumber(prev)
	c, cok := versionNumber(cur)
	return pok && cok && c == p+1
}

func versionNumber(v string) (int, bool) {
	n := 0
	if v == "" {
		return 0, false
	}
	for _, r := range v {
		if r < '0' || r > '9' {
			return 0, false
		}
		n = n*10 + int(r-'0')
	}
	return n, true
}
