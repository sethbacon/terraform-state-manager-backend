//go:build integration

package maintenance_test

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
	"github.com/terraform-state-manager/terraform-state-manager/internal/maintenance"
)

// AGAINST A REAL POSTGRESQL, because every property here is a property of the
// SQL rather than of the Go.
//
// A mock returns whatever its fixture declares, so it cannot tell an UPDATE that
// moved the right rows from one that moved all of them, cannot evaluate the
// `IS DISTINCT FROM` that makes the derive step idempotent, cannot enforce the
// foreign keys that make an orphan an orphan, and cannot roll a transaction
// back. sqlmock tests the Go; this tests the SQL.

const reownTestDB = "tsm_reown_maintenance_test"

const (
	orgDefault = "11111111-1111-4111-8111-111111111111"
	orgTwo     = "22222222-2222-4222-8222-222222222222"
	orgGhost   = "33333333-3333-4333-8333-333333333333" // never inserted
)

func newReownDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping PostgreSQL integration test")
	}
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = admin.Close() }()
	if err := admin.Ping(); err != nil {
		t.Skipf("TEST_DATABASE_URL unreachable (%v); skipping", err)
	}
	for _, stmt := range []string{
		`DROP DATABASE IF EXISTS ` + pgx.Identifier{reownTestDB}.Sanitize() + ` WITH (FORCE)`,
		`CREATE DATABASE ` + pgx.Identifier{reownTestDB}.Sanitize(),
	} {
		if _, err := admin.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	parsed.Path = "/" + reownTestDB
	db, err := sql.Open("pgx", parsed.String())
	if err != nil {
		t.Fatalf("open %s: %v", parsed.Redacted(), err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := appdb.RunMigrations(db, "up"); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	return db
}

func exec(t *testing.T, db *sql.DB, stmts ...string) {
	t.Helper()
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
}

func ownerOfRow(t *testing.T, db *sql.DB, table, id string) string {
	t.Helper()
	var owner sql.NullString
	if err := db.QueryRow(`SELECT organization_id::text FROM `+table+` WHERE id = $1`, id).Scan(&owner); err != nil {
		t.Fatalf("owner of %s/%s: %v", table, id, err)
	}
	return owner.String
}

// knownOrgs is the destination checker the CLI backs with the identity schema.
//
// The app connection has NO organizations table -- they live in identity, and
// 000033 gives the partition no foreign key into it because identity may be a
// different database. An early version of this suite queried `organizations` on
// the app connection and failed with 42P01, which is what surfaced that the
// check belonged behind an injected dependency rather than in the SQL.
func knownOrgs(ids ...string) maintenance.DestinationExists {
	set := map[string]bool{}
	for _, id := range ids {
		set[id] = true
	}
	return func(_ context.Context, id string) (bool, error) { return set[id], nil }
}

// seedEstate builds the shape the deployment is actually in: every row sitting
// at the default organization.
func seedEstate(t *testing.T, db *sql.DB) {
	t.Helper()
	exec(t, db,
		`INSERT INTO state_sources (id, name, type, config, organization_id) VALUES
		   ('aaaa0000-0000-4000-8000-000000000001','src-a','local','{}','`+orgDefault+`'),
		   ('aaaa0000-0000-4000-8000-000000000002','src-b','local','{}','`+orgDefault+`')`,
		`INSERT INTO state_transfers (id, mode, source_id, source_key, target_source_id, target_key, status, organization_id) VALUES
		   ('bbbb0000-0000-4000-8000-000000000001','backup',
		    'aaaa0000-0000-4000-8000-000000000001','k',
		    'aaaa0000-0000-4000-8000-000000000002','k2','success','`+orgDefault+`')`,
	)
}

func TestIntegration_Move_ReownsConfigRootsAndDerivesDependents(t *testing.T) {
	db := newReownDB(t)
	seedEstate(t, db)

	res, err := maintenance.Move(context.Background(), db, orgDefault, orgTwo, knownOrgs(orgDefault, orgTwo))
	if err != nil {
		t.Fatalf("Move: %v", err)
	}
	if res.Moved["state_sources"] != 2 {
		t.Errorf("state_sources moved = %d, want 2", res.Moved["state_sources"])
	}
	for _, id := range []string{"aaaa0000-0000-4000-8000-000000000001", "aaaa0000-0000-4000-8000-000000000002"} {
		if got := ownerOfRow(t, db, "state_sources", id); got != orgTwo {
			t.Errorf("state_sources %s owner = %s, want %s", id, got, orgTwo)
		}
	}
	// The transfer followed its parent rather than being moved by the mapping.
	if got := ownerOfRow(t, db, "state_transfers", "bbbb0000-0000-4000-8000-000000000001"); got != orgTwo {
		t.Errorf("state_transfers owner = %s, want %s -- the derive step did not run "+
			"or ran before its parent moved", got, orgTwo)
	}
}

// TestIntegration_Move_IsIdempotent matters because an operator who is unsure
// whether the command completed will run it again. A second run must be a no-op,
// not a second migration.
func TestIntegration_Move_IsIdempotent(t *testing.T) {
	db := newReownDB(t)
	seedEstate(t, db)
	ctx := context.Background()

	if _, err := maintenance.Move(ctx, db, orgDefault, orgTwo, knownOrgs(orgDefault, orgTwo)); err != nil {
		t.Fatalf("first Move: %v", err)
	}
	second, err := maintenance.Move(ctx, db, orgDefault, orgTwo, knownOrgs(orgDefault, orgTwo))
	if err != nil {
		t.Fatalf("second Move: %v", err)
	}
	for table, n := range second.Moved {
		if n != 0 {
			t.Errorf("second run moved %d row(s) in %s; it should be a no-op", n, table)
		}
	}
}

// TestIntegration_Move_LeavesOtherOrganizationsAlone is the containment
// property: a mapping names a source organization, and rows outside it are not
// swept along.
func TestIntegration_Move_LeavesOtherOrganizationsAlone(t *testing.T) {
	db := newReownDB(t)
	seedEstate(t, db)
	exec(t, db, `INSERT INTO state_sources (id, name, type, config, organization_id) VALUES
	   ('cccc0000-0000-4000-8000-000000000009','already-twos','local','{}','`+orgTwo+`')`)

	// Move in the OTHER direction: two -> default. Only the one row above may move.
	res, err := maintenance.Move(context.Background(), db, orgTwo, orgDefault, knownOrgs(orgDefault, orgTwo))
	if err != nil {
		t.Fatalf("Move: %v", err)
	}
	if res.Moved["state_sources"] != 1 {
		t.Fatalf("state_sources moved = %d, want exactly 1 -- the predicate is not "+
			"restricting to the source organization", res.Moved["state_sources"])
	}
}

// TestIntegration_Move_RefusesAnUnknownDestination. Stamping rows into an
// organization that names nothing produces well-formed rows invisible to every
// tenant and visible only to a platform admin -- the exact failure #436 exists
// to close, reached through the tool meant to repair it.
func TestIntegration_Move_RefusesAnUnknownDestination(t *testing.T) {
	db := newReownDB(t)
	seedEstate(t, db)

	if _, err := maintenance.Move(context.Background(), db, orgDefault, orgGhost, knownOrgs(orgDefault, orgTwo)); err == nil {
		t.Fatal("Move accepted a destination organization that does not exist")
	}
	// ...and nothing moved: the refusal is before the writes, and the
	// transaction covers the case where it is not.
	if got := ownerOfRow(t, db, "state_sources", "aaaa0000-0000-4000-8000-000000000001"); got != orgDefault {
		t.Errorf("rows moved despite the refusal: owner = %s", got)
	}
}

func TestIntegration_Move_RefusesAnUnnamedMapping(t *testing.T) {
	db := newReownDB(t)
	seedEstate(t, db)
	ctx := context.Background()

	for _, tc := range []struct{ from, to, why string }{
		{"", orgTwo, "no source organization"},
		{orgDefault, "", "no destination organization"},
		{orgDefault, orgDefault, "source and destination are the same"},
	} {
		if _, err := maintenance.Move(ctx, db, tc.from, tc.to, knownOrgs(orgDefault, orgTwo)); err == nil {
			t.Errorf("Move accepted a mapping with %s", tc.why)
		}
	}
	if got := ownerOfRow(t, db, "state_sources", "aaaa0000-0000-4000-8000-000000000001"); got != orgDefault {
		t.Errorf("a refused mapping still moved rows: owner = %s", got)
	}

	// A nil checker is refused rather than skipped: "could not confirm the
	// destination" and "confirmed it" must not have the same outcome.
	if _, err := maintenance.Move(ctx, db, orgDefault, orgTwo, nil); err == nil {
		t.Error("Move ran with no way to confirm the destination organization exists")
	}
}

// TestIntegration_Census_IsReadOnly. Verify is the mode an operator reaches for
// first, on a production database, to decide whether to run the other one.
func TestIntegration_Census_IsReadOnly(t *testing.T) {
	db := newReownDB(t)
	seedEstate(t, db)

	before := ownerOfRow(t, db, "state_sources", "aaaa0000-0000-4000-8000-000000000001")
	res, err := maintenance.Census(context.Background(), db)
	if err != nil {
		t.Fatalf("Census: %v", err)
	}
	if len(res.Rows) == 0 {
		t.Fatal("census reported nothing on a seeded estate")
	}
	if after := ownerOfRow(t, db, "state_sources", "aaaa0000-0000-4000-8000-000000000001"); after != before {
		t.Errorf("census changed ownership: %s -> %s", before, after)
	}
}

// TestIntegration_Move_ReportsWhenItEmptiesTheDefaultOrganization.
//
// The default organization is where things land when nothing else decides: a
// first login not covered by a group mapping is placed there, and every
// partition root's column DEFAULT is tsm_default_organization_id(). Moving the
// estate away from it without saying so leaves an operator with new users
// arriving in an organization that owns nothing, and no signal that it happened.
//
// The move deliberately does NOT repoint the setting -- that is a separate
// operator decision -- so REPORTING is the whole contract, and this is what
// pins it.
func TestIntegration_Move_ReportsWhenItEmptiesTheDefaultOrganization(t *testing.T) {
	db := newReownDB(t)
	seedEstate(t, db)
	exec(t, db, `UPDATE system_settings SET default_organization_id = '`+orgDefault+`'::uuid WHERE id = 1`)

	res, err := maintenance.Move(context.Background(), db, orgDefault, orgTwo, knownOrgs(orgDefault, orgTwo))
	if err != nil {
		t.Fatalf("Move: %v", err)
	}
	if !res.MovedAwayFromDefault {
		t.Error("moving the estate OUT of the default organization was not reported")
	}
	if res.DefaultOrganizationID != orgDefault {
		t.Errorf("DefaultOrganizationID = %q, want %q", res.DefaultOrganizationID, orgDefault)
	}
	if !strings.Contains(res.String(), "WARNING") {
		t.Errorf("the report does not warn:\n%s", res.String())
	}
}

// TestIntegration_Move_DoesNotWarnWhenTheDefaultIsUnaffected is the control:
// a warning on every move is a warning nobody reads.
func TestIntegration_Move_DoesNotWarnWhenTheDefaultIsUnaffected(t *testing.T) {
	db := newReownDB(t)
	seedEstate(t, db)
	// The deployment default is org TWO; the move is out of the other one.
	exec(t, db, `UPDATE system_settings SET default_organization_id = '`+orgTwo+`'::uuid WHERE id = 1`)

	res, err := maintenance.Move(context.Background(), db, orgDefault, orgTwo, knownOrgs(orgDefault, orgTwo))
	if err != nil {
		t.Fatalf("Move: %v", err)
	}
	if res.MovedAwayFromDefault {
		t.Error("a move that did not touch the default organization was reported as if it had")
	}
	if strings.Contains(res.String(), "WARNING") {
		t.Errorf("the report warns when it should not:\n%s", res.String())
	}
}
