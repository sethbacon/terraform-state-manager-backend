//go:build integration

// THE EQUIVALENCE PROOF — sethbacon/terraform-state-manager-backend#393 Phase 2b.
//
// isolation_integration_test.go, beside this file, proves the leak: List and
// GetByID return every organization's rows because they cannot express an
// organization. This file is the other half of Phase 2's contract, which
// migration 000033 states as "dual-reads behind a flag and proves equivalence".
// It stands the scoped readers up against the unscoped ones on a real PostgreSQL
// and answers the two questions Phase 3 is gated on:
//
//   - On a deployment with ONE organization, do they return the same rows? If
//     not, flipping the reads would begin hiding rows from the tenant that owns
//     them, and #393's fix would land as data loss.
//   - On a deployment with TWO, does the scoped reader return strictly fewer?
//     If not, the predicate is not narrowing anything and the flip would be
//     ceremony.
//
// IT DOES NOT TOUCH THE TRIPWIRE. isolation_integration_test.go calls List(ctx)
// and GetByID(ctx, id) at their current signatures on purpose, so that Phase 3
// breaks the build and forces its assertions to be inverted. Phase 2b adds
// readers beside them and changes neither, so that file still compiles and still
// records what is broken. The moment it stops compiling is the moment the leak
// is closed, and that moment is not this one.
//
// Run with:
//
//	TEST_DATABASE_URL='postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable' \
//	  go test -tags integration ./internal/tenancy/...
package tenancy

import (
	"context"
	"database/sql"
	"reflect"
	"testing"

	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
	"github.com/terraform-state-manager/terraform-state-manager/internal/tenantscope"
)

// seedUnownedSource writes a state_sources row with NO organization_id — the
// column default is tsm_default_organization_id(), which returns NULL until the
// startup backfill populates the carrier. That is not a hypothetical row: it is
// every row on an upgraded deployment before its first successful boot, and
// every row a replica still running the previous build writes after it.
// seedUnownedSource writes a source with organization_id NULL.
//
// AFTER PHASE 4 THE SCHEMA FORBIDS THIS, so the helper relaxes the constraint for
// the length of the test and restores it. That is deliberate, not a workaround:
// the read layer's refusal to hand an unowned row to a tenant is DEFENCE IN
// DEPTH, and defence in depth is exactly what must keep working when the layer
// above it is absent.
//
// The state is still reachable in practice -- restoring a backup taken before
// 000034 produces exactly these rows -- and a `= ANY($1::uuid[])` predicate that
// stopped excluding NULL would hand every one of them to whichever tenant asked
// first. Deleting this test because the column is NOT NULL now would remove the
// only check on that.
func seedUnownedSource(t *testing.T, db *sql.DB, name string) string {
	t.Helper()
	if _, err := db.Exec(`ALTER TABLE state_sources ALTER COLUMN organization_id DROP NOT NULL`); err != nil {
		t.Fatalf("relax NOT NULL to seed an unowned source: %v", err)
	}
	t.Cleanup(func() {
		// Restored even on failure: a later test in this package would otherwise
		// run against a schema this one quietly weakened.
		_, _ = db.Exec(`DELETE FROM state_sources WHERE organization_id IS NULL`)
		_, _ = db.Exec(`ALTER TABLE state_sources ALTER COLUMN organization_id SET NOT NULL`)
	})
	var id string
	if err := db.QueryRow(
		`INSERT INTO state_sources (name, type, organization_id) VALUES ($1, 'local', NULL) RETURNING id`,
		name).Scan(&id); err != nil {
		t.Fatalf("seed unowned source %q: %v", name, err)
	}
	var org sql.NullString
	if err := db.QueryRow(`SELECT organization_id FROM state_sources WHERE id = $1`, id).Scan(&org); err != nil {
		t.Fatalf("read org of %q: %v", name, err)
	}
	if org.Valid {
		t.Fatalf("the unowned fixture was stamped with organization %s; this test is about the "+
			"rows the backfill has not reached and there are none", org.String)
	}
	return id
}

// seedRichSourceInOrg writes a row with EVERY column populated — endpoint,
// config, scope, and an encrypted_credentials blob.
//
// It exists because the obvious fixture does not test what it looks like it
// tests. seedSourceInOrg (isolation_integration_test.go) writes only name, type
// and organization_id, leaving endpoint NULL, config/scope at their defaults and
// encrypted_credentials NULL — so a comparison of two Source values built from
// those rows agrees on the untouched fields no matter WHAT the scoped query
// selected into them. A scoped reader that returned `NULL::bytea` in place of
// encrypted_credentials passed the equivalence test unchanged, which was
// discovered by mutating the reader and watching the suite stay green.
//
// The credential blob is the field that matters most here. It is what
// ConnectSource decrypts (internal/api/sources.go), so a scoped reader that
// returned the right rows with an empty credential would not leak anything — it
// would break every connector in the deployment at the moment Phase 3 flipped
// the reads, and would do it silently until the first backend call failed.
func seedRichSourceInOrg(t *testing.T, db *sql.DB, orgID, name string) string {
	t.Helper()
	var id string
	err := db.QueryRow(
		`INSERT INTO state_sources (name, type, endpoint, config, scope, encrypted_credentials, organization_id)
		 VALUES ($1, 'http', $2, $3::jsonb, $4::jsonb, $5, $6) RETURNING id`,
		name,
		"https://state.example.internal/tfstate",
		`{"base_path":"/data","timeout":30}`,
		`{"workspaces":["prod","staging"]}`,
		[]byte{0x01, 0x02, 0x03, 0xfe, 0xff},
		orgID).Scan(&id)
	if err != nil {
		t.Fatalf("seed rich source %q in org %s: %v", name, orgID, err)
	}
	return id
}

func sourceIDs(sources []repositories.Source) []string {
	ids := make([]string, 0, len(sources))
	for _, s := range sources {
		ids = append(ids, s.ID)
	}
	return ids
}

// TestIntegration_ScopedSourceReads_AreEquivalentInOneOrganization is the
// evidence that Phase 3 is safe on the deployment shape almost every TSM install
// actually has: one organization, the `default` one bootstrap.Run creates.
//
// It compares the WHOLE Source values, not just the ids. The scoped reader is a
// second SELECT over the same columns, and a column quietly dropped from it —
// encrypted_credentials, say — would return the right rows with the wrong
// contents, which is the kind of divergence an id comparison passes straight
// over and which surfaces later as a connector that cannot authenticate.
func TestIntegration_ScopedSourceReads_AreEquivalentInOneOrganization(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	ids := []string{
		seedSourceInOrg(t, db, orgAlpha, "alpha-one"),
		seedSourceInOrg(t, db, orgAlpha, "alpha-two"),
		// At least one row with every column populated, or the comparison below
		// is vacuous on the columns no fixture ever writes. See
		// seedRichSourceInOrg.
		seedRichSourceInOrg(t, db, orgAlpha, "alpha-three"),
	}
	repo := repositories.NewSourceRepository(db)
	scope := tenantscope.Scope{OrgIDs: []string{orgAlpha}}

	unscoped, err := repo.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(unscoped) != len(ids) {
		t.Fatalf("List returned %d sources, want %d — the fixture is wrong, not the system",
			len(unscoped), len(ids))
	}

	// THE GUARD ON THE GUARD. reflect.DeepEqual below can only object to a field
	// that some fixture actually populated, so assert that the fixture did —
	// otherwise a future simplification of the seeding turns the whole
	// equivalence proof into a comparison of default values that agrees with
	// anything.
	assertFixtureExercisesEveryColumn(t, unscoped)

	scoped, err := repo.ListInScope(ctx, scope)
	if err != nil {
		t.Fatalf("ListInScope: %v", err)
	}
	if !reflect.DeepEqual(unscoped, scoped) {
		t.Fatalf("the two readers disagree on a single-organization deployment.\n"+
			"  unscoped: %v\n  scoped:   %v\n"+
			"Phase 3 would flip reads onto the second of these, so this is rows disappearing "+
			"from the tenant that owns them.", sourceIDs(unscoped), sourceIDs(scoped))
	}

	for _, id := range ids {
		want, err := repo.GetByID(ctx, id)
		if err != nil || want == nil {
			t.Fatalf("GetByID(%s): %v %+v", id, err, want)
		}
		got, err := repo.GetByIDInScope(ctx, id, scope)
		if err != nil {
			t.Fatalf("GetByIDInScope(%s): %v", id, err)
		}
		if got == nil {
			t.Fatalf("GetByIDInScope withheld %s (%q) from the organization that owns it", id, want.Name)
		}
		if !reflect.DeepEqual(want, got) {
			t.Fatalf("GetByIDInScope returned a different row for %s:\n  unscoped: %+v\n  scoped:   %+v", id, want, got)
		}
	}

	t.Logf("PROVED: on a single-organization deployment SourceRepository.List and ListInScope "+
		"returned the same %d rows, and GetByID and GetByIDInScope agreed on every one of them. "+
		"This is the equivalence Phase 3 is gated on.", len(ids))
}

// TestIntegration_ScopedSourceReads_WithholdAnotherOrganization is the leak,
// measured. It is the same fixture isolation_integration_test.go uses to prove
// the exposure, read through the scoped readers instead.
func TestIntegration_ScopedSourceReads_WithholdAnotherOrganization(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	alphaID := seedSourceInOrg(t, db, orgAlpha, "alpha-production")
	betaID := seedSourceInOrg(t, db, orgBeta, "beta-production")

	repo := repositories.NewSourceRepository(db)
	scope := tenantscope.Scope{OrgIDs: []string{orgAlpha}}

	unscoped, err := repo.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	scoped, err := repo.ListInScope(ctx, scope)
	if err != nil {
		t.Fatalf("ListInScope: %v", err)
	}

	if len(scoped) >= len(unscoped) {
		t.Fatalf("ListInScope returned %d of %d rows for a caller in one of two organizations; "+
			"the predicate is not narrowing anything (%v vs %v)",
			len(scoped), len(unscoped), sourceIDs(scoped), sourceIDs(unscoped))
	}
	if len(scoped) != 1 || scoped[0].ID != alphaID {
		t.Fatalf("ListInScope returned %v, want exactly the Alpha source %s", sourceIDs(scoped), alphaID)
	}

	// THE READ #393 NAMES AS THE HIGHEST BLAST RADIUS. /sources/:id/state/* feeds
	// GetByID straight into credential decryption, so a row returned here is a
	// credential one tenant can decrypt of another's.
	leaked, err := repo.GetByID(ctx, betaID)
	if err != nil || leaked == nil {
		t.Fatalf("GetByID(beta) — the premise of this test is that it still leaks: %v %+v", err, leaked)
	}
	withheld, err := repo.GetByIDInScope(ctx, betaID, scope)
	if err != nil {
		t.Fatalf("GetByIDInScope(beta): %v", err)
	}
	if withheld != nil {
		t.Fatalf("GetByIDInScope returned organization Beta's source %q to a caller scoped to "+
			"organization Alpha — the scoped reader does not close the leak it exists to close",
			withheld.Name)
	}

	t.Logf("PROVED: with two organizations seeded, ListInScope returned %d of %d rows and "+
		"GetByIDInScope refused Beta's source %s, which GetByID returned. The %d withheld rows "+
		"are what one tenant can currently read of another's.",
		len(scoped), len(unscoped), betaID, len(unscoped)-len(scoped))
}

// TestIntegration_ScopedSourceReads_FailClosed covers the direction that
// actually matters when something has gone wrong. A caller whose tenancy could
// not be established must read NOTHING — not everything, which is what a
// predicate that degrades to a no-op would give them.
func TestIntegration_ScopedSourceReads_FailClosed(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	alphaID := seedSourceInOrg(t, db, orgAlpha, "alpha-production")
	seedSourceInOrg(t, db, orgBeta, "beta-production")
	repo := repositories.NewSourceRepository(db)

	cases := []struct {
		name  string
		scope tenantscope.Scope
	}{
		{
			// The zero value: no principal, an unwired resolver, or a principal
			// with no qualifying membership. Every failure path in
			// internal/tenantscope returns this.
			name:  "the zero scope",
			scope: tenantscope.Scope{},
		},
		{
			// Resolved, and names an organization that owns nothing here.
			name:  "an organization with no rows",
			scope: tenantscope.Scope{OrgIDs: []string{"33333333-3333-4333-8333-333333333333"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rows, err := repo.ListInScope(ctx, tc.scope)
			if err != nil {
				t.Fatalf("ListInScope: %v", err)
			}
			if len(rows) != 0 {
				t.Fatalf("ListInScope returned %d rows (%v) for a scope that permits nothing",
					len(rows), sourceIDs(rows))
			}
			got, err := repo.GetByIDInScope(ctx, alphaID, tc.scope)
			if err != nil {
				t.Fatalf("GetByIDInScope: %v", err)
			}
			if got != nil {
				t.Fatalf("GetByIDInScope returned %q for a scope that permits nothing", got.Name)
			}
		})
	}
}

// TestIntegration_ScopedSourceReads_UnownedRowsBelongToNoTenant pins the
// treatment of organization_id IS NULL, which is the one behaviour of this
// predicate that cannot be read off the Go source.
//
// `NULL = ANY(ARRAY[...])` evaluates to NULL, never to true, so an unstamped row
// is invisible to every tenant — which is the rule tenantscope.Scope.Permits
// applies to the empty owner, for the reason it gives: on these tables NULL
// means "no tenant has been asserted", not "belongs to everyone", and admitting
// such rows to everybody would leak whichever tenant owns the most of them.
//
// This is asserted against a real PostgreSQL because it is a fact about
// PostgreSQL. A mock cannot tell you what NULL does inside ANY, and a reviewer
// who assumed it behaved like a plain IN list would have been right about the
// syntax and wrong about the security property.
func TestIntegration_ScopedSourceReads_UnownedRowsBelongToNoTenant(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	unownedID := seedUnownedSource(t, db, "written-by-the-previous-build")
	ownedID := seedSourceInOrg(t, db, orgAlpha, "alpha-production")
	repo := repositories.NewSourceRepository(db)

	tenant := tenantscope.Scope{OrgIDs: []string{orgAlpha}}
	rows, err := repo.ListInScope(ctx, tenant)
	if err != nil {
		t.Fatalf("ListInScope: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != ownedID {
		t.Fatalf("ListInScope returned %v; an unstamped row must not fall to a tenant that "+
			"happens to be asking", sourceIDs(rows))
	}
	got, err := repo.GetByIDInScope(ctx, unownedID, tenant)
	if err != nil {
		t.Fatalf("GetByIDInScope(unowned): %v", err)
	}
	if got != nil {
		t.Fatalf("GetByIDInScope handed the unstamped source %q to organization Alpha", got.Name)
	}

	// The platform admin is the one principal that is genuinely platform-wide,
	// and is therefore the only one that can see a row no tenant owns — which is
	// also the only way such a row is ever noticed before the backfill stamps it.
	admin := tenantscope.Scope{PlatformAdmin: true}
	all, err := repo.ListInScope(ctx, admin)
	if err != nil {
		t.Fatalf("ListInScope(platform admin): %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("a platform admin saw %v, want both sources including the unstamped one", sourceIDs(all))
	}
	if got, err := repo.GetByIDInScope(ctx, unownedID, admin); err != nil || got == nil {
		t.Fatalf("GetByIDInScope(unowned, platform admin) = %+v, %v; want the row", got, err)
	}

	t.Logf("PROVED: state_sources %s has organization_id NULL and was withheld from organization "+
		"%s by both scoped readers, while the platform admin saw it. NULL = ANY(...) is NULL, "+
		"not true.", unownedID, orgAlpha)
}

// assertFixtureExercisesEveryColumn fails when no seeded row carries a non-zero
// value for a column, because a column that is zero on BOTH sides of an
// equivalence comparison is a column the comparison is not testing.
//
// Written as an assertion rather than a comment because it already went wrong
// once: the first version of this file compared three rows that had no endpoint,
// no credentials and empty config, and a scoped reader mutated to select
// `NULL::bytea` for encrypted_credentials passed it.
func assertFixtureExercisesEveryColumn(t *testing.T, sources []repositories.Source) {
	t.Helper()
	var endpoint, creds, config, scopeCol bool
	for _, s := range sources {
		endpoint = endpoint || s.Endpoint != ""
		creds = creds || len(s.EncryptedCredentials) > 0
		config = config || len(s.Config) > 0
		scopeCol = scopeCol || len(s.Scope) > 0
	}
	for _, c := range []struct {
		column string
		seeded bool
	}{
		{"endpoint", endpoint},
		{"encrypted_credentials", creds},
		{"config", config},
		{"scope", scopeCol},
	} {
		if !c.seeded {
			t.Fatalf("no seeded source carries a %s, so the equivalence comparison cannot "+
				"detect a scoped reader that stopped selecting it", c.column)
		}
	}
}
