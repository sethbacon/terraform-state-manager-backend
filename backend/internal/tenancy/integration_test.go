//go:build integration

// Migration 000033 and the backfill, against a real PostgreSQL.
//
// The guards in internal/db/migration_tenancy_test.go are text guards: they
// prove the migration and tenancy.PartitionedTables agree with each other. They
// cannot prove the migration APPLIES, that the column default actually fires, or
// that the down leg's ordering survives contact with the dependency graph — and
// those are exactly the three things that would take a deployment down.
//
// WHY THE DEFAULT NEEDS A DATABASE TO VERIFY. `ALTER TABLE ... ALTER COLUMN ...
// SET DEFAULT tsm_default_organization_id()` binds to the function's OID at ALTER
// time. Whether that resolves, whether a STABLE sql function is even permitted in
// a column default, and whether the value it returns is re-read per INSERT rather
// than frozen at ALTER time are all properties of PostgreSQL, not of the text. A
// default frozen at ALTER time would return NULL forever — the migration runs
// before the carrier is ever populated — and would look identical in review.
//
// Run with:
//
//	TEST_DATABASE_URL='postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable' \
//	  go test -tags integration ./internal/tenancy/...
//
// TEST_DATABASE_URL is the variable the three existing build-tagged suites use,
// and CI's `Postgres-backed tests` job sets it (.github/workflows/ci.yml). The
// TestIntegration prefix is also deliberate: that job's anti-vacuity check greps
// the transcript for a passing test matching `TestIntegration`, so these run
// under the same assertion that the tagged set actually built.
package tenancy

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	_ "github.com/jackc/pgx/v5/stdlib"

	appdb "github.com/terraform-state-manager/terraform-state-manager/internal/db"
)

// Its own database, for the reason internal/bootstrap's and internal/approles'
// suites each have one: package test binaries run concurrently and this suite
// drops and recreates its schema.
const testDatabaseName = "tsm_tenancy_test"

// inheritingTables are the seven that deliberately DO NOT get an
// organization_id: each carries source_id NOT NULL with ON DELETE CASCADE, so
// its organization is its source's, derivable by a join that cannot return NULL.
//
// Pinned here so the decision is enforced rather than merely documented. Adding
// the column to one of these is not a harmless belt-and-braces improvement — it
// creates a SECOND answer to "whose is this row", and the copy is the one that
// goes stale, in a predicate that will decide who may read a Terraform state
// file.
var inheritingTables = []string{
	"state_backups",
	"state_edits",
	"state_locks",
	"state_analyses",
	"source_sync_status",
	"state_analysis_history",
	"state_module_refs",
}

func newTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping PostgreSQL integration test")
	}
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open %s: %v", dsn, err)
	}
	defer func() { _ = admin.Close() }()
	if err := admin.Ping(); err != nil {
		t.Skipf("TEST_DATABASE_URL is not reachable (%v); skipping", err)
	}
	for _, stmt := range []string{
		`DROP DATABASE IF EXISTS ` + pgx.Identifier{testDatabaseName}.Sanitize() + ` WITH (FORCE)`,
		`CREATE DATABASE ` + pgx.Identifier{testDatabaseName}.Sanitize(),
	} {
		if _, err := admin.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}

	// url.Parse rather than pgx.ParseConfig + re-render: swapping only the path
	// preserves the port, TLS mode and every other parameter the operator set. The
	// same approach internal/bootstrap's suite takes, for the same reason.
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse TEST_DATABASE_URL: %v", err)
	}
	parsed.Path = "/" + testDatabaseName
	db, err := sql.Open("pgx", parsed.String())
	if err != nil {
		t.Fatalf("open %s: %v", parsed.Redacted(), err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := appdb.RunMigrations(db, "up"); err != nil {
		t.Fatalf("app migrations: %v", err)
	}
	return db
}

// TestIntegration_OrganizationPartition_ColumnShape asserts on the CATALOG
// rather than on behaviour, because it is the only way to cover all nine tables
// without nine hand-written INSERTs that each have to satisfy a different set of
// NOT NULLs and CHECK constraints. A test that covered two tables and inferred
// the rest is how a table gets left out.
//
// Three properties, and each is a distinct failure:
//   - the column exists (the migration applied)
//   - it is NULLABLE (phase 1 is non-breaking; a NOT NULL here is phase 4's
//     breaking change smuggled in early, and it would reject every INSERT made
//     before the carrier is populated — including the ones made during the boot
//     that populates it)
//   - it defaults to tsm_default_organization_id() (writes are stamped)
func TestIntegration_OrganizationPartition_ColumnShape(t *testing.T) {
	db := newTestDB(t)

	for _, table := range PartitionedTables {
		var nullable, colDefault sql.NullString
		err := db.QueryRow(`
			SELECT is_nullable, column_default
			  FROM information_schema.columns
			 WHERE table_schema = current_schema()
			   AND table_name = $1 AND column_name = 'organization_id'`, table).
			Scan(&nullable, &colDefault)
		if err == sql.ErrNoRows {
			t.Errorf("%s has no organization_id column after 000033", table)
			continue
		}
		if err != nil {
			t.Fatalf("inspect %s: %v", table, err)
		}
		if nullable.String != "YES" {
			t.Errorf("%s.organization_id is NOT NULL; phase 1 must be non-breaking and the column "+
				"is NULL on every row until the startup backfill runs", table)
		}
		if !colDefault.Valid || colDefault.String != "tsm_default_organization_id()" {
			t.Errorf("%s.organization_id default = %q, want tsm_default_organization_id() — "+
				"without it every INSERT writes NULL", table, colDefault.String)
		}
	}

	for _, table := range inheritingTables {
		var n int
		if err := db.QueryRow(`
			SELECT count(*) FROM information_schema.columns
			 WHERE table_schema = current_schema()
			   AND table_name = $1 AND column_name = 'organization_id'`, table).Scan(&n); err != nil {
			t.Fatalf("inspect %s: %v", table, err)
		}
		if n != 0 {
			t.Errorf("%s has its own organization_id. It inherits one through source_id NOT NULL "+
				"ON DELETE CASCADE, and a second copy can disagree with the first — see 000033's "+
				"closing comment before adding this back.", table)
		}
	}
}

// TestIntegration_OrganizationPartition_BackfillStampsAndDefaults is the
// behavioural half: the full phase-1 lifecycle on one root table
// (state_sources) and one nullable-parent run table (drift_runs).
func TestIntegration_OrganizationPartition_BackfillStampsAndDefaults(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	const orgID = "11111111-2222-3333-4444-555555555555"

	// A row written BEFORE the carrier is populated — which is every row on an
	// upgraded deployment, and any row a replica writes between the migration
	// applying and bootstrap.Run completing.
	var preSourceID string
	if err := db.QueryRow(
		`INSERT INTO state_sources (name, type) VALUES ('pre-backfill', 'local') RETURNING id`).
		Scan(&preSourceID); err != nil {
		t.Fatalf("insert a pre-backfill source: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO drift_runs (status, callback_token) VALUES ('completed', 'tok-pre')`); err != nil {
		t.Fatalf("insert a pre-backfill drift run: %v", err)
	}

	// It must be NULL, not an error and not some invented value. If this row came
	// out already stamped, the default was frozen to something at ALTER time and
	// the rest of this test would be meaningless.
	if got := orgOf(t, db, "state_sources", "name = 'pre-backfill'"); got.Valid {
		t.Fatalf("a source written before the backfill carries organization_id %q; "+
			"it must be NULL — the carrier is empty at this point", got.String)
	}

	// An empty id is refused rather than written. Asserted on the MESSAGE, not
	// merely on err != nil: without the refusal the sweep runs and Postgres
	// rejects ''::uuid, so a bare non-nil check passes with the guard deleted.
	// The load-bearing version of this assertion is
	// TestBackfill_RefusesAnEmptyOrganizationIDBeforeTouchingTheDatabase, which
	// proves no statement is sent at all; this one keeps the two in step.
	if err := Backfill(ctx, db, ""); err == nil ||
		!strings.Contains(err.Error(), "empty default organization id") {
		t.Fatalf("Backfill with an empty organization id: err = %v, want the refusal", err)
	}

	if err := Backfill(ctx, db, orgID); err != nil {
		t.Fatalf("Backfill: %v", err)
	}

	// 1. The pre-existing rows are stamped.
	for _, c := range []struct{ table, where string }{
		{"state_sources", "name = 'pre-backfill'"},
		{"drift_runs", "callback_token = 'tok-pre'"},
	} {
		got := orgOf(t, db, c.table, c.where)
		if !got.Valid || got.String != orgID {
			t.Errorf("%s after backfill: organization_id = %v, want %s", c.table, got, orgID)
		}
	}

	// 2. The carrier is readable, and the function reads it. This is what the
	// column defaults resolve through.
	var fn sql.NullString
	if err := db.QueryRow(`SELECT tsm_default_organization_id()::text`).Scan(&fn); err != nil {
		t.Fatalf("tsm_default_organization_id(): %v", err)
	}
	if !fn.Valid || fn.String != orgID {
		t.Fatalf("tsm_default_organization_id() = %v, want %s", fn, orgID)
	}

	// 3. THE POINT OF THE WHOLE MECHANISM: an INSERT that never mentions
	// organization_id is stamped anyway. This statement is the shape every
	// repository in internal/db/repositories uses today, unmodified — which is
	// what makes "written on create" true for write paths nobody edited.
	if _, err := db.Exec(
		`INSERT INTO state_sources (name, type) VALUES ('post-backfill', 'local')`); err != nil {
		t.Fatalf("insert a post-backfill source: %v", err)
	}
	got := orgOf(t, db, "state_sources", "name = 'post-backfill'")
	if !got.Valid || got.String != orgID {
		t.Errorf("a source inserted after the backfill, by a statement that never names the column, "+
			"has organization_id = %v, want %s. The default is not firing, so every write between "+
			"now and phase 4 depends on a backfill catching it later.", got, orgID)
	}

	// 4. Idempotent: the second run changes nothing and errors on nothing.
	if err := Backfill(ctx, db, orgID); err != nil {
		t.Fatalf("second Backfill: %v", err)
	}
	got = orgOf(t, db, "state_sources", "name = 'post-backfill'")
	if !got.Valid || got.String != orgID {
		t.Errorf("re-running Backfill changed organization_id to %v", got)
	}
}

// TestIntegration_OrganizationPartition_DownMigrationApplies proves the rollback
// ORDER, which is the one thing about 000033's down leg that can fail.
//
// A column default is a catalogued dependency on the function it calls, so
// DROP FUNCTION before the columns fails with "cannot drop function ... because
// other objects depend on it" — and golang-migrate marks the version DIRTY on a
// failed step, blocking every subsequent up AND down until an operator clears
// the flag by hand. That is a worse outcome than having no down migration, and
// no amount of reading the file proves it does not happen.
func TestIntegration_OrganizationPartition_DownMigrationApplies(t *testing.T) {
	db := newTestDB(t)

	if err := Backfill(context.Background(), db, "11111111-2222-3333-4444-555555555555"); err != nil {
		t.Fatalf("Backfill: %v", err)
	}
	// Roll the whole schema down and back up: golang-migrate has no
	// single-version step here, and a full round trip also proves 000033 can be
	// RE-APPLIED over its own rollback.
	if err := appdb.RunMigrations(db, "down"); err != nil {
		t.Fatalf("migrating down: %v", err)
	}
	if err := appdb.RunMigrations(db, "up"); err != nil {
		t.Fatalf("re-applying after the rollback: %v", err)
	}

	// Back to the phase-1 shape, with the carrier cleared: a rollback that left
	// the old default organization behind would silently stamp the next boot's
	// rows before bootstrap.Run had confirmed that organization still exists.
	var colDefault sql.NullString
	if err := db.QueryRow(`
		SELECT column_default FROM information_schema.columns
		 WHERE table_schema = current_schema()
		   AND table_name = 'state_sources' AND column_name = 'organization_id'`).Scan(&colDefault); err != nil {
		t.Fatalf("inspect state_sources after the round trip: %v", err)
	}
	if !colDefault.Valid || colDefault.String != "tsm_default_organization_id()" {
		t.Errorf("after down+up the default is %q, want tsm_default_organization_id()", colDefault.String)
	}
	var carrier sql.NullString
	if err := db.QueryRow(`SELECT default_organization_id::text FROM system_settings WHERE id = 1`).Scan(&carrier); err != nil {
		t.Fatalf("read the carrier after the round trip: %v", err)
	}
	if carrier.Valid {
		t.Errorf("the carrier survived a rollback as %q; DROP COLUMN should have taken it", carrier.String)
	}
}

func orgOf(t *testing.T, db *sql.DB, table, where string) sql.NullString {
	t.Helper()
	var got sql.NullString
	// table and where are test-local literals, never input.
	if err := db.QueryRow(`SELECT organization_id::text FROM ` + table + ` WHERE ` + where).Scan(&got); err != nil {
		t.Fatalf("read %s.organization_id WHERE %s: %v", table, where, err)
	}
	return got
}
