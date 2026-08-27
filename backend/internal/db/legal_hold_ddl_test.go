package db

// legal_hold_ddl_test.go keeps migration 000035 identical to the shape the
// shared module's sweep expects (#373).
//
// The module exposes store.LegalHoldTableDDL, and running it at startup would
// need no migration at all -- which is what terraform-registry-backend did, and
// it was a defect: a table created at boot lands wherever the handle points, so
// the schema the migration chain describes and the schema the database has
// become different things.
//
// Transcribing the DDL into a numbered migration fixes that and creates a new
// risk in its place: the transcript can drift from the source. This closes it.
// If the module changes the hold table's shape, this fails and names the
// difference, instead of the divergence being discovered by a sweep that
// silently exempts nothing.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	idstore "github.com/sethbacon/terraform-suite-identity/identity/store"
)

// legalHoldTable is the name migration 000035 creates, and the name the
// application passes to store.WithLegalHolds.
//
// SCHEMA-QUALIFIED, deliberately. The sweep runs on the identity pool, whose
// search_path is "identity,public" -- so an unqualified name would resolve to
// identity.legal_holds if one ever appeared, and the hold API would write one
// table while the sweep read another. Qualifying it makes that unreachable.
const legalHoldTable = "public.legal_holds"

func TestMigrationMatchesTheModulesHoldTableDDL(t *testing.T) {
	want, err := idstore.LegalHoldTableDDL(legalHoldTable)
	if err != nil {
		t.Fatalf("LegalHoldTableDDL: %v", err)
	}
	got, err := os.ReadFile(filepath.Join("migrations", "000035_legal_holds.up.sql"))
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}

	for _, frag := range ddlFragments(want) {
		if !strings.Contains(normaliseSQL(string(got)), frag) {
			t.Errorf("migration 000035 does not contain this part of the module's DDL:\n\n  %s\n\n"+
				"The sweep reads the columns this DDL declares. A migration that drifts from it "+
				"produces a table the exemption cannot use -- and the sweep would then exempt "+
				"nothing while reporting success (#373).", frag)
		}
	}
}

// TestMigrationDeclaresNoExtraColumns is the other direction.
//
// An extra column is harmless to the sweep but means the migration is no longer
// a transcript of anything, which is how the two start diverging in the
// direction that DOES matter.
func TestMigrationDeclaresNoExtraColumns(t *testing.T) {
	want, err := idstore.LegalHoldTableDDL(legalHoldTable)
	if err != nil {
		t.Fatalf("LegalHoldTableDDL: %v", err)
	}
	got, err := os.ReadFile(filepath.Join("migrations", "000035_legal_holds.up.sql"))
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	wantCols, gotCols := columnNames(want), columnNames(string(got))
	if len(wantCols) == 0 {
		t.Fatal("no columns parsed from the module's DDL; this test is checking nothing")
	}
	for col := range gotCols {
		if !wantCols[col] {
			t.Errorf("migration 000035 declares column %q, which the module's DDL does not. "+
				"The migration is meant to be a transcript.", col)
		}
	}
	for col := range wantCols {
		if !gotCols[col] {
			t.Errorf("migration 000035 is missing column %q from the module's DDL", col)
		}
	}
}

// TestTheHoldTableNameIsSchemaQualified pins the decision that makes shadowing
// impossible.
func TestTheHoldTableNameIsSchemaQualified(t *testing.T) {
	if !strings.Contains(legalHoldTable, ".") {
		t.Fatal("the hold table name is unqualified.\n" +
			"The sweep runs on the identity pool with search_path=identity,public, so an " +
			"unqualified name resolves to identity.legal_holds if one ever exists -- the hold API " +
			"would write one table and the sweep would read another, every hold would look " +
			"placed, and the sweep would delete the rows anyway.")
	}
	if schema := strings.SplitN(legalHoldTable, ".", 2)[0]; schema != "public" {
		t.Errorf("the hold table is qualified to %q; migration 000035 creates it in public", schema)
	}
}

var (
	wsRun      = regexp.MustCompile(`\s+`)
	columnDecl = regexp.MustCompile(`(?m)^\s*([a-z_][a-z0-9_]*)\s+(UUID|TEXT|TIMESTAMPTZ|BOOLEAN)\b`)
)

func normaliseSQL(s string) string { return wsRun.ReplaceAllString(s, " ") }

// ddlFragments splits the module's DDL into the pieces worth comparing,
// whitespace-normalised so indentation is not a difference.
func ddlFragments(ddl string) []string {
	var out []string
	for _, m := range columnDecl.FindAllStringSubmatch(ddl, -1) {
		out = append(out, normaliseSQL(m[1]+" "+m[2]))
	}
	if strings.Contains(ddl, "CONSTRAINT legal_holds_range") {
		out = append(out, "CONSTRAINT legal_holds_range CHECK (end_date >= start_date)")
	}
	return out
}

func columnNames(ddl string) map[string]bool {
	out := map[string]bool{}
	for _, m := range columnDecl.FindAllStringSubmatch(ddl, -1) {
		out[m[1]] = true
	}
	return out
}
