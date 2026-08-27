//go:build integration

// THE PHASE 3 READ FLIP FOR schedules, against a real PostgreSQL — #393.
//
// scoped_read_integration_test.go beside this file does the same job for
// state_sources and explains at length why these questions cannot be answered
// with a mock: `NULL = ANY(ARRAY[...])` is a fact about PostgreSQL, and a mock
// asked about it will tell you whatever the person writing the mock believed.
//
// Three properties, and each is a separate claim:
//
//   - on a single-organization deployment the scoped readers return exactly what
//     the unscoped ones did, so the flip is not rows disappearing from the
//     tenant that owns them;
//   - with two organizations the scoped readers return strictly fewer, so the
//     predicate is narrowing something rather than being ceremony;
//   - a scope that permits nothing reads nothing, and an unstamped row belongs
//     to no tenant at all.
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

// seedScheduleInOrg writes one schedules row owned by orgID and returns its id.
//
// EVERY COLUMN THE READER PROJECTS IS POPULATED, which is not fussiness: a
// fixture that leaves last_run_at, last_run_id and last_status NULL makes a
// reflect.DeepEqual comparison of two Schedule values agree on those fields no
// matter what the scoped query selected into them. The equivalent shortcut in
// the state_sources fixture hid a dropped encrypted_credentials column, and was
// only found by mutating the reader and watching the suite stay green.
//
// target_config carries a real pipeline id for the same reason and one more:
// it is the field a firing dispatches on, so a scoped reader that returned the
// right rows with an empty target_config would not leak anything — it would
// break every schedule in the deployment, silently, at the moment the reads
// flipped.
//
// Raw SQL rather than ScheduleRepository.Create because the subject is the READ
// path; going through the writer would make this depend on whether the writer
// stamps the column, which is a different question with its own test.
func seedScheduleInOrg(t *testing.T, db *sql.DB, orgID, name string) string {
	t.Helper()
	var id string
	err := db.QueryRow(`
		INSERT INTO schedules
			(name, cron_expr, target_type, target_config, enabled,
			 last_run_at, next_run_at, last_run_id, last_status, organization_id)
		VALUES ($1, '0 2 * * *', 'drift', $2::jsonb, true,
			 now() - interval '1 day', now() + interval '1 day',
			 gen_random_uuid(), 'success', $3)
		RETURNING id`,
		name,
		`{"pipeline_connection_id":"6f1c2d3e-4a5b-4c6d-8e9f-0a1b2c3d4e5f","working_dir":"envs/prod"}`,
		orgID).Scan(&id)
	if err != nil {
		t.Fatalf("seed schedule %q in org %s: %v", name, orgID, err)
	}
	return id
}

func scheduleIDs(items []repositories.Schedule) []string {
	ids := make([]string, 0, len(items))
	for _, s := range items {
		ids = append(ids, s.ID)
	}
	return ids
}

// assertScheduleFixtureIsNotVacuous is the guard on the guard. reflect.DeepEqual
// can only object to a field some fixture actually populated, so assert that the
// fixture did — otherwise a later simplification of the seeding turns the
// equivalence proof below into a comparison of zero values that agrees with
// anything.
func assertScheduleFixtureIsNotVacuous(t *testing.T, items []repositories.Schedule) {
	t.Helper()
	for _, s := range items {
		switch {
		case s.LastRunAt == nil:
			t.Fatal("the fixture left last_run_at NULL, so the comparison below cannot see that column")
		case s.NextRunAt == nil:
			t.Fatal("the fixture left next_run_at NULL, so the comparison below cannot see that column")
		case s.LastRunID == nil:
			t.Fatal("the fixture left last_run_id NULL, so the comparison below cannot see that column")
		case s.LastStatus == nil:
			t.Fatal("the fixture left last_status NULL, so the comparison below cannot see that column")
		case len(s.TargetConfig) < 3:
			t.Fatal("the fixture left target_config empty, so a reader that dropped it would pass")
		case s.OrganizationID == "":
			t.Fatal("the fixture left organization_id unstamped, so the predicate has nothing to match")
		}
	}
}

// TestIntegration_ScopedScheduleReads_AreEquivalentInOneOrganization is the
// evidence the flip is safe on the deployment shape almost every TSM install
// has: one organization, the `default` one bootstrap.Run creates.
func TestIntegration_ScopedScheduleReads_AreEquivalentInOneOrganization(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	ids := []string{
		seedScheduleInOrg(t, db, orgAlpha, "alpha-nightly"),
		seedScheduleInOrg(t, db, orgAlpha, "alpha-weekly"),
	}
	repo := repositories.NewScheduleRepository(db)
	scope := tenantscope.Scope{OrgIDs: []string{orgAlpha}}

	unscoped, err := repo.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(unscoped) != len(ids) {
		t.Fatalf("List returned %d schedules, want %d — the fixture is wrong, not the system",
			len(unscoped), len(ids))
	}
	assertScheduleFixtureIsNotVacuous(t, unscoped)

	scoped, err := repo.ListInScope(ctx, scope)
	if err != nil {
		t.Fatalf("ListInScope: %v", err)
	}
	if !reflect.DeepEqual(unscoped, scoped) {
		t.Fatalf("the two readers disagree on a single-organization deployment.\n"+
			"  unscoped: %v\n  scoped:   %v\n"+
			"The flip serves the second of these, so this is rows disappearing from the tenant "+
			"that owns them.", scheduleIDs(unscoped), scheduleIDs(scoped))
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
			t.Fatalf("GetByIDInScope returned a different row for %s:\n  unscoped: %+v\n  scoped:   %+v",
				id, want, got)
		}
	}

	t.Logf("PROVED: on a single-organization deployment ScheduleRepository.List and ListInScope "+
		"returned the same %d rows, and GetByID and GetByIDInScope agreed on every one.", len(ids))
}

// TestIntegration_ScopedScheduleReads_WithholdAnotherOrganization is the leak on
// this root, measured.
//
// The by-id half is the one that matters most. RunSchedule loads a schedule by
// id and dispatches its target_config stamped with the SCHEDULE's organization,
// so an unscoped load is one tenant executing another tenant's schedule on that
// tenant's pipeline connection — an execution, not a disclosure.
func TestIntegration_ScopedScheduleReads_WithholdAnotherOrganization(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	alphaID := seedScheduleInOrg(t, db, orgAlpha, "alpha-nightly")
	betaID := seedScheduleInOrg(t, db, orgBeta, "beta-nightly")

	repo := repositories.NewScheduleRepository(db)
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
			len(scoped), len(unscoped), scheduleIDs(scoped), scheduleIDs(unscoped))
	}
	if len(scoped) != 1 || scoped[0].ID != alphaID {
		t.Fatalf("ListInScope returned %v, want exactly the Alpha schedule %s",
			scheduleIDs(scoped), alphaID)
	}

	leaked, err := repo.GetByID(ctx, betaID)
	if err != nil || leaked == nil {
		t.Fatalf("GetByID(beta) — the premise of this test is that the unscoped reader still "+
			"returns it: %v %+v", err, leaked)
	}
	withheld, err := repo.GetByIDInScope(ctx, betaID, scope)
	if err != nil {
		t.Fatalf("GetByIDInScope(beta): %v", err)
	}
	if withheld != nil {
		t.Fatalf("GetByIDInScope returned organization Beta's schedule %q to a caller scoped to "+
			"organization Alpha; RunSchedule would then have dispatched it on Beta's pipeline "+
			"connection", withheld.Name)
	}

	t.Logf("PROVED: with two organizations seeded, ListInScope returned %d of %d rows and "+
		"GetByIDInScope refused Beta's schedule %s, which GetByID returned.",
		len(scoped), len(unscoped), betaID)
}

// TestIntegration_ScopedScheduleReads_FailClosed covers the direction that
// matters when something has gone wrong. A caller whose tenancy could not be
// established reads NOTHING — not everything, which is what a predicate that
// degraded to a no-op would give them.
func TestIntegration_ScopedScheduleReads_FailClosed(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	alphaID := seedScheduleInOrg(t, db, orgAlpha, "alpha-nightly")
	seedScheduleInOrg(t, db, orgBeta, "beta-nightly")
	repo := repositories.NewScheduleRepository(db)

	for _, tc := range []struct {
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
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows, err := repo.ListInScope(ctx, tc.scope)
			if err != nil {
				t.Fatalf("ListInScope: %v", err)
			}
			if len(rows) != 0 {
				t.Fatalf("ListInScope returned %d rows (%v) for a scope that permits nothing",
					len(rows), scheduleIDs(rows))
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

// TestIntegration_ScopedScheduleReads_UnownedRowsBelongToNoTenant pins the
// treatment of organization_id IS NULL, the one behaviour of this predicate that
// cannot be read off the Go source.
//
// Migration 000034 made the column NOT NULL, so the constraint is relaxed for
// the length of this test and restored afterwards. That is deliberate rather
// than a workaround: the read layer's refusal to hand an unstamped row to a
// tenant is DEFENCE IN DEPTH, and defence in depth is exactly what has to keep
// working when the layer above it is absent — a database restored from a backup
// taken before 000034 holds precisely these rows.
func TestIntegration_ScopedScheduleReads_UnownedRowsBelongToNoTenant(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	seedScheduleInOrg(t, db, orgAlpha, "alpha-nightly")

	if _, err := db.Exec(`ALTER TABLE schedules ALTER COLUMN organization_id DROP NOT NULL`); err != nil {
		t.Fatalf("relax NOT NULL to seed an unowned schedule: %v", err)
	}
	t.Cleanup(func() {
		// Restored even on failure: a later test in this package would otherwise
		// run against a schema this one quietly weakened.
		_, _ = db.Exec(`DELETE FROM schedules WHERE organization_id IS NULL`)
		_, _ = db.Exec(`ALTER TABLE schedules ALTER COLUMN organization_id SET NOT NULL`)
	})

	var unownedID string
	if err := db.QueryRow(
		`INSERT INTO schedules (name, cron_expr, organization_id) VALUES ('orphan', 'daily', NULL) RETURNING id`,
	).Scan(&unownedID); err != nil {
		t.Fatalf("seed unowned schedule: %v", err)
	}

	repo := repositories.NewScheduleRepository(db)

	// Invisible to a tenant...
	tenant := tenantscope.Scope{OrgIDs: []string{orgAlpha}}
	rows, err := repo.ListInScope(ctx, tenant)
	if err != nil {
		t.Fatalf("ListInScope: %v", err)
	}
	for _, s := range rows {
		if s.ID == unownedID {
			t.Fatal("ListInScope handed an unstamped schedule to a tenant. `NULL = ANY(...)` must " +
				"never be true: on this table NULL means 'no tenant has been asserted', not " +
				"'belongs to everyone', and admitting these rows leaks whichever tenant owns the " +
				"most of them.")
		}
	}
	if got, err := repo.GetByIDInScope(ctx, unownedID, tenant); err != nil || got != nil {
		t.Fatalf("GetByIDInScope handed an unstamped schedule to a tenant: %+v %v", got, err)
	}

	// ...and still reachable by the one principal that is deployment-wide, which
	// is what keeps such a row repairable rather than merely lost.
	admin, err := repo.ListInScope(ctx, tenantscope.Scope{PlatformAdmin: true})
	if err != nil {
		t.Fatalf("ListInScope(platform admin): %v", err)
	}
	found := false
	for _, s := range admin {
		if s.ID == unownedID {
			found = true
		}
	}
	if !found {
		t.Fatal("a platform admin could not see the unstamped schedule. They are the only " +
			"principal who can, so this row would be invisible to everybody and unfixable " +
			"through the API.")
	}
}
