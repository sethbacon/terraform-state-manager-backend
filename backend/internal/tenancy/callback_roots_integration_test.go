//go:build integration

// THE PHASE 3 READ FLIP AND THE CALLBACK AUTHORITY FOR THE THREE ROOTS THAT
// HAVE BOTH — drift_runs, health_runs, drift_records (#393 option B).
//
// scoped_read_integration_test.go and schedule_scoped_read_integration_test.go
// beside this file do the read half for state_sources and schedules, and explain
// at length why a mock cannot answer these questions: `NULL = ANY(ARRAY[...])`
// is a fact about PostgreSQL, and a mock asked about it tells you whatever the
// person writing the mock believed. That warning was earned — replacing a
// derived authority with a permissive platform-admin scope compiled and left the
// entire sqlmock dispatch suite green.
//
// This file adds the half those two do not have, because their roots do not have
// it: a SECOND kind of caller with no principal at all. A CI job posts its plan
// result holding a per-run bearer token, and the authority it acts under is
// derived from the run that token authenticates — tenancy.SystemActingIn, the
// same constructor the scheduler uses, producing the same Scope type the request
// middleware resolves.
//
// # How the two halves of that claim are covered, and why they are separate
//
// The derivation itself — "a token that authenticates yields the run's
// organization, one that does not yields no authority, and never the
// platform-admin bypass" — is asserted directly on the function that performs
// it, in internal/api/callback_authority_test.go. It cannot be asserted here:
// internal/api imports this package, so this package cannot import it back.
//
// What is asserted HERE is the other half, and it is the half only a real
// database can answer: given the authority that derivation produces, does the
// SQL actually refuse another organization's row? Every scope below is built by
// calling SystemActingIn on the owning run — the exact call the callback makes —
// rather than assembled by hand, so a change that made the derivation confer the
// wrong thing would show up in both places rather than neither.
//
// EVERY REFUSAL HERE HAS ITS CONTROL. The same row is read back under its
// rightful owner's authority in the same test, because a refusal and a reader
// that returns nothing to anybody are otherwise indistinguishable.
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

// callbackScope builds the authority a machine callback derives for a run — via
// SystemActingIn, never by hand. A test that assembled tenantscope.Scope{OrgIDs:
// ...} itself would still pass if the derivation started conferring something
// else entirely, which is the gap this whole file exists to avoid leaving.
func callbackScope(t *testing.T, orgID, table, runID string) tenantscope.Scope {
	t.Helper()
	sys, err := SystemActingIn(orgID, table, runID)
	if err != nil {
		t.Fatalf("SystemActingIn(%s, %s/%s): %v", orgID, table, runID, err)
	}
	if sys.Scope().PlatformAdmin {
		t.Fatal("a callback derived a PLATFORM ADMIN scope. That carrier takes the unfiltered " +
			"branch of every InScope reader, so every assertion below would pass while the " +
			"callback read the whole deployment.")
	}
	return sys.Scope()
}

// seedDriftRunInOrg writes one drift_runs row and returns its id.
//
// EVERY COLUMN THE READER PROJECTS IS POPULATED. A fixture that left the result
// columns NULL would make a reflect.DeepEqual of two DriftRun values agree on
// them no matter what the scoped query selected — the shortcut that hid a
// dropped encrypted_credentials column in the state_sources fixture, found only
// by mutating the reader and watching the suite stay green.
func seedDriftRunInOrg(t *testing.T, db *sql.DB, orgID, sourceID, stateKey, token string) string {
	t.Helper()
	var id string
	err := db.QueryRow(`
		INSERT INTO drift_runs
			(pipeline_connection_id, source_id, state_key, repo_ref, working_dir, status,
			 added, changed, destroyed, drifted, summary, detail, callback_token, actor,
			 truncated, omitted_entries, omitted_attrs, unparseable, unmasked, organization_id)
		VALUES (NULL, $1, $2, 'refs/heads/main', 'envs/prod', 'completed',
			 1, 2, 3, true, '[{"address":"aws_s3_bucket.b","actions":["update"]}]'::jsonb,
			 'ok', $3, 'alice', true, 4, 5, false, true, $4)
		RETURNING id::text`, nullableID(sourceID), stateKey, token, orgID).Scan(&id)
	if err != nil {
		t.Fatalf("seed drift_run in %s: %v", orgID, err)
	}
	return id
}

// seedHealthRunInOrg writes one health_runs row and returns its id.
func seedHealthRunInOrg(t *testing.T, db *sql.DB, orgID, token string) string {
	t.Helper()
	var id string
	err := db.QueryRow(`
		INSERT INTO health_runs
			(pipeline_connection_id, repo_ref, working_dir, terraform_version, provider_versions,
			 module_versions, registry_host, status, init_ok, plan_ok, success, summary, detail,
			 callback_token, actor, organization_id)
		VALUES (NULL, 'refs/heads/main', 'envs/prod', '1.9.5', '{"aws":"5.60.0"}'::jsonb,
			 '{"vpc":"5.1.0"}'::jsonb, 'registry.example.test', 'completed', true, true, true,
			 '{"note":"ok"}'::jsonb, 'ok', $1, 'alice', $2)
		RETURNING id::text`, token, orgID).Scan(&id)
	if err != nil {
		t.Fatalf("seed health_run in %s: %v", orgID, err)
	}
	return id
}

// seedDriftRecordInOrg writes one drift_records row, owned by orgID and pointing
// at sourceID.
func seedDriftRecordInOrg(t *testing.T, db *sql.DB, orgID, sourceID, stateKey string) string {
	t.Helper()
	var id string
	err := db.QueryRow(`
		INSERT INTO drift_records
			(source_id, state_key, pipeline_connection_id, last_run_id, origin, severity,
			 added, changed, destroyed, summary, status, acknowledged_by, ack_note,
			 external_ref, detections, truncated, omitted_entries, omitted_attrs,
			 unparseable, unmasked, organization_id)
		VALUES ($1, $2, NULL, NULL, 'run', 'critical',
			 1, 2, 3, '[{"address":"aws_db_instance.main","actions":["delete"]}]'::jsonb,
			 'open', '', '', NULL, 2, true, 6, 7, false, true, $3)
		RETURNING id::text`, nullableID(sourceID), stateKey, orgID).Scan(&id)
	if err != nil {
		t.Fatalf("seed drift_record in %s: %v", orgID, err)
	}
	return id
}

// nullableID renders "" as a SQL NULL, so a fixture can seed the parentless rows
// 000033 says the callback roots' own column exists for.
func nullableID(id string) any {
	if id == "" {
		return nil
	}
	return id
}

func driftRunIDs(items []repositories.DriftRun) []string {
	out := make([]string, 0, len(items))
	for _, r := range items {
		out = append(out, r.ID)
	}
	return out
}

func healthRunIDs(items []repositories.HealthRun) []string {
	out := make([]string, 0, len(items))
	for _, r := range items {
		out = append(out, r.ID)
	}
	return out
}

func driftRecordIDs(items []repositories.DriftRecord) []string {
	out := make([]string, 0, len(items))
	for _, r := range items {
		out = append(out, r.ID)
	}
	return out
}

// assertDriftRunFixtureIsNotVacuous is the guard on the guard: reflect.DeepEqual
// can only object to a field the fixture actually populated.
func assertDriftRunFixtureIsNotVacuous(t *testing.T, items []repositories.DriftRun) {
	t.Helper()
	for _, r := range items {
		switch {
		case r.Added == nil || r.Changed == nil || r.Destroyed == nil || r.Drifted == nil:
			t.Fatal("the fixture left a result column NULL, so the comparison below cannot see it")
		case len(r.Summary) < 3:
			t.Fatal("the fixture left summary empty, so a reader that dropped it would pass")
		case r.OmittedEntries == 0 || r.OmittedAttrs == 0:
			t.Fatal("the fixture left the completeness markers at their zero values")
		case r.OrganizationID == "":
			t.Fatal("the fixture left organization_id unstamped, so the predicate has nothing to match")
		}
	}
}

// ---------------------------------------------------------------------------
// The request-facing half: scoped reads on all three roots.
// ---------------------------------------------------------------------------

// TestIntegration_ScopedCallbackRootReads_AreEquivalentInOneOrganization is the
// evidence the flip is safe on the deployment shape almost every TSM install
// has: one organization, the `default` one bootstrap.Run creates.
func TestIntegration_ScopedCallbackRootReads_AreEquivalentInOneOrganization(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	srcA := seedSourceInOrg(t, db, orgAlpha, "alpha-equiv")
	runID := seedDriftRunInOrg(t, db, orgAlpha, srcA, "envs/prod.tfstate", "tok-a")
	healthID := seedHealthRunInOrg(t, db, orgAlpha, "htok-a")
	recordID := seedDriftRecordInOrg(t, db, orgAlpha, srcA, "envs/prod.tfstate")

	scope := tenantscope.Scope{OrgIDs: []string{orgAlpha}}
	drift := repositories.NewDriftRepository(db)
	health := repositories.NewHealthRepository(db)
	records := repositories.NewDriftRecordRepository(db)

	unscopedRuns, err := drift.List(ctx, 0, 0, repositories.DriftRunFilter{})
	if err != nil {
		t.Fatalf("drift List: %v", err)
	}
	if len(unscopedRuns) != 1 {
		t.Fatalf("drift List returned %d runs, want 1 — the fixture is wrong, not the system", len(unscopedRuns))
	}
	assertDriftRunFixtureIsNotVacuous(t, unscopedRuns)
	scopedRuns, err := drift.ListInScope(ctx, 0, 0, repositories.DriftRunFilter{}, scope)
	if err != nil {
		t.Fatalf("drift ListInScope: %v", err)
	}
	if !reflect.DeepEqual(unscopedRuns, scopedRuns) {
		t.Fatalf("the two drift-run readers disagree on a single-organization deployment:\n"+
			"  unscoped: %v\n  scoped:   %v\nThe flip serves the second, so this is rows "+
			"disappearing from the tenant that owns them.", driftRunIDs(unscopedRuns), driftRunIDs(scopedRuns))
	}

	unscopedHealth, err := health.List(ctx, 0, 0, "")
	if err != nil {
		t.Fatalf("health List: %v", err)
	}
	scopedHealth, err := health.ListInScope(ctx, 0, 0, "", scope)
	if err != nil {
		t.Fatalf("health ListInScope: %v", err)
	}
	if len(unscopedHealth) != 1 || !reflect.DeepEqual(unscopedHealth, scopedHealth) {
		t.Fatalf("the two health-run readers disagree:\n  unscoped: %v\n  scoped:   %v",
			healthRunIDs(unscopedHealth), healthRunIDs(scopedHealth))
	}

	unscopedRecs, err := records.List(ctx, nil, "", "", 0, 0, nil, nil)
	if err != nil {
		t.Fatalf("records List: %v", err)
	}
	scopedRecs, err := records.ListInScope(ctx, nil, "", "", 0, 0, nil, nil, scope)
	if err != nil {
		t.Fatalf("records ListInScope: %v", err)
	}
	if len(unscopedRecs) != 1 || !reflect.DeepEqual(unscopedRecs, scopedRecs) {
		t.Fatalf("the two drift-record readers disagree:\n  unscoped: %v\n  scoped:   %v",
			driftRecordIDs(unscopedRecs), driftRecordIDs(scopedRecs))
	}

	// ...and the by-id readers agree row for row, including every projected
	// column. A scoped query that dropped one would return a row that compared
	// equal on ids alone.
	wantRun, err := drift.GetByID(ctx, runID)
	if err != nil || wantRun == nil {
		t.Fatalf("drift GetByID: %v %+v", err, wantRun)
	}
	gotRun, err := drift.GetByIDInScope(ctx, runID, scope)
	if err != nil || gotRun == nil {
		t.Fatalf("drift GetByIDInScope withheld %s from the organization that owns it: %v", runID, err)
	}
	if !reflect.DeepEqual(wantRun, gotRun) {
		t.Fatalf("drift GetByIDInScope returned a different row:\n  unscoped: %+v\n  scoped:   %+v", wantRun, gotRun)
	}

	wantHealth, err := health.GetByID(ctx, healthID)
	if err != nil || wantHealth == nil {
		t.Fatalf("health GetByID: %v %+v", err, wantHealth)
	}
	gotHealth, err := health.GetByIDInScope(ctx, healthID, scope)
	if err != nil || gotHealth == nil {
		t.Fatalf("health GetByIDInScope withheld %s from its owner: %v", healthID, err)
	}
	if !reflect.DeepEqual(wantHealth, gotHealth) {
		t.Fatalf("health GetByIDInScope returned a different row:\n  unscoped: %+v\n  scoped:   %+v", wantHealth, gotHealth)
	}

	wantRec, err := records.GetByID(ctx, recordID)
	if err != nil || wantRec == nil {
		t.Fatalf("records GetByID: %v %+v", err, wantRec)
	}
	gotRec, err := records.GetByIDInScope(ctx, recordID, scope)
	if err != nil || gotRec == nil {
		t.Fatalf("records GetByIDInScope withheld %s from its owner: %v", recordID, err)
	}
	if !reflect.DeepEqual(wantRec, gotRec) {
		t.Fatalf("records GetByIDInScope returned a different row:\n  unscoped: %+v\n  scoped:   %+v", wantRec, gotRec)
	}

	t.Logf("PROVED: on a single-organization deployment the scoped and unscoped readers agreed " +
		"on drift_runs, health_runs and drift_records, both listed and by id.")
}

// TestIntegration_ScopedCallbackRootReads_WithholdAnotherOrganization is the
// leak on these three roots, measured.
func TestIntegration_ScopedCallbackRootReads_WithholdAnotherOrganization(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	srcA := seedSourceInOrg(t, db, orgAlpha, "alpha-withhold")
	srcB := seedSourceInOrg(t, db, orgBeta, "beta-withhold")
	runA := seedDriftRunInOrg(t, db, orgAlpha, srcA, "alpha.tfstate", "tok-a")
	runB := seedDriftRunInOrg(t, db, orgBeta, srcB, "beta.tfstate", "tok-b")
	healthA := seedHealthRunInOrg(t, db, orgAlpha, "htok-a")
	healthB := seedHealthRunInOrg(t, db, orgBeta, "htok-b")
	recA := seedDriftRecordInOrg(t, db, orgAlpha, srcA, "alpha.tfstate")
	recB := seedDriftRecordInOrg(t, db, orgBeta, srcB, "beta.tfstate")

	scopeA := tenantscope.Scope{OrgIDs: []string{orgAlpha}}
	drift := repositories.NewDriftRepository(db)
	health := repositories.NewHealthRepository(db)
	records := repositories.NewDriftRecordRepository(db)

	runs, err := drift.ListInScope(ctx, 0, 0, repositories.DriftRunFilter{}, scopeA)
	if err != nil {
		t.Fatalf("drift ListInScope: %v", err)
	}
	if len(runs) != 1 || runs[0].ID != runA {
		t.Fatalf("drift ListInScope returned %v, want exactly Alpha's run %s", driftRunIDs(runs), runA)
	}
	if n, err := drift.CountRunsInScope(ctx, repositories.DriftRunFilter{}, scopeA); err != nil || n != 1 {
		t.Fatalf("drift CountRunsInScope = %d, %v; want 1. An unscoped total beside a scoped page "+
			"reports how many runs the rest of the deployment has.", n, err)
	}

	hruns, err := health.ListInScope(ctx, 0, 0, "", scopeA)
	if err != nil {
		t.Fatalf("health ListInScope: %v", err)
	}
	if len(hruns) != 1 || hruns[0].ID != healthA {
		t.Fatalf("health ListInScope returned %v, want exactly Alpha's run %s", healthRunIDs(hruns), healthA)
	}
	if n, err := health.CountRunsInScope(ctx, "", scopeA); err != nil || n != 1 {
		t.Fatalf("health CountRunsInScope = %d, %v; want 1", n, err)
	}

	recs, err := records.ListInScope(ctx, nil, "", "", 0, 0, nil, nil, scopeA)
	if err != nil {
		t.Fatalf("records ListInScope: %v", err)
	}
	if len(recs) != 1 || recs[0].ID != recA {
		t.Fatalf("records ListInScope returned %v, want exactly Alpha's record %s", driftRecordIDs(recs), recA)
	}
	// The status chips are on the same predicate: an unscoped tally beside a
	// scoped list says "2 open" to a tenant who can see one of them.
	counts, err := records.CountsByStatusInScope(ctx, scopeA)
	if err != nil {
		t.Fatalf("records CountsByStatusInScope: %v", err)
	}
	if counts["open"] != 1 {
		t.Fatalf("records CountsByStatusInScope = %v; want one open record, not the deployment's total", counts)
	}

	// The by-id half, with its control every time: Beta's row is refused to
	// Alpha AND still served to Beta.
	for _, tc := range []struct {
		name string
		read func(scope tenantscope.Scope) (bool, error)
	}{
		{"drift_runs", func(s tenantscope.Scope) (bool, error) {
			got, err := drift.GetByIDInScope(ctx, runB, s)
			return got != nil, err
		}},
		{"health_runs", func(s tenantscope.Scope) (bool, error) {
			got, err := health.GetByIDInScope(ctx, healthB, s)
			return got != nil, err
		}},
		{"drift_records", func(s tenantscope.Scope) (bool, error) {
			got, err := records.GetByIDInScope(ctx, recB, s)
			return got != nil, err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			served, err := tc.read(scopeA)
			if err != nil {
				t.Fatalf("read under Alpha: %v", err)
			}
			if served {
				t.Errorf("organization Beta's %s row was served under organization Alpha's scope", tc.name)
			}
			// THE CONTROL. Without it a reader that returns nothing to anybody
			// passes the assertion above, and refusal and brokenness are the same
			// green.
			served, err = tc.read(tenantscope.Scope{OrgIDs: []string{orgBeta}})
			if err != nil {
				t.Fatalf("read under Beta: %v", err)
			}
			if !served {
				t.Errorf("organization Beta cannot read its OWN %s row: the refusal above is not "+
					"evidence of scoping, only of a reader that returns nothing", tc.name)
			}
		})
	}
}

// TestIntegration_ScopedCallbackRootReads_FailClosed covers the direction that
// matters when something has gone wrong: a caller whose tenancy could not be
// established reads NOTHING, not everything. It also covers the four Phase 4a
// coverage/summary readers (LatestPerStateInScope, LiveByStateInScope,
// CountsBySourceInScope, CountIncompleteInScope), reusing the source and
// record already seeded below.
func TestIntegration_ScopedCallbackRootReads_FailClosed(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	srcA := seedSourceInOrg(t, db, orgAlpha, "alpha-failclosed")
	runA := seedDriftRunInOrg(t, db, orgAlpha, srcA, "alpha.tfstate", "tok-a")
	healthA := seedHealthRunInOrg(t, db, orgAlpha, "htok-a")
	recA := seedDriftRecordInOrg(t, db, orgAlpha, srcA, "alpha.tfstate")

	drift := repositories.NewDriftRepository(db)
	health := repositories.NewHealthRepository(db)
	records := repositories.NewDriftRecordRepository(db)

	for _, tc := range []struct {
		name  string
		scope tenantscope.Scope
	}{
		// The zero value: no principal, an unwired resolver, or a principal with
		// no qualifying membership. Every failure path in internal/tenantscope
		// returns this — and so does an unauthenticated machine callback.
		{"the zero scope", tenantscope.Scope{}},
		// Resolved, and names an organization that owns nothing here.
		{"an organization with no rows", tenantscope.Scope{OrgIDs: []string{"33333333-3333-4333-8333-333333333333"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if rows, err := drift.ListInScope(ctx, 0, 0, repositories.DriftRunFilter{}, tc.scope); err != nil || len(rows) != 0 {
				t.Errorf("drift ListInScope returned %v (%v) for a scope that permits nothing", driftRunIDs(rows), err)
			}
			if got, err := drift.GetByIDInScope(ctx, runA, tc.scope); err != nil || got != nil {
				t.Errorf("drift GetByIDInScope returned %+v (%v) for a scope that permits nothing", got, err)
			}
			if rows, err := health.ListInScope(ctx, 0, 0, "", tc.scope); err != nil || len(rows) != 0 {
				t.Errorf("health ListInScope returned %v (%v) for a scope that permits nothing", healthRunIDs(rows), err)
			}
			if got, err := health.GetByIDInScope(ctx, healthA, tc.scope); err != nil || got != nil {
				t.Errorf("health GetByIDInScope returned %+v (%v) for a scope that permits nothing", got, err)
			}
			if rows, err := records.ListInScope(ctx, nil, "", "", 0, 0, nil, nil, tc.scope); err != nil || len(rows) != 0 {
				t.Errorf("records ListInScope returned %v (%v) for a scope that permits nothing", driftRecordIDs(rows), err)
			}
			if got, err := records.GetByIDInScope(ctx, recA, tc.scope); err != nil || got != nil {
				t.Errorf("records GetByIDInScope returned %+v (%v) for a scope that permits nothing", got, err)
			}
			if out, err := drift.LatestPerStateInScope(ctx, srcA, tc.scope); err != nil || len(out) != 0 {
				t.Errorf("drift LatestPerStateInScope returned %v (%v) for a scope that permits nothing", out, err)
			}
			if out, err := records.LiveByStateInScope(ctx, srcA, tc.scope); err != nil || len(out) != 0 {
				t.Errorf("records LiveByStateInScope returned %v (%v) for a scope that permits nothing", out, err)
			}
			if out, err := records.CountsBySourceInScope(ctx, tc.scope); err != nil || len(out) != 0 {
				t.Errorf("records CountsBySourceInScope returned %v (%v) for a scope that permits nothing", out, err)
			}
			if n, err := records.CountIncompleteInScope(ctx, tc.scope); err != nil || n != 0 {
				t.Errorf("records CountIncompleteInScope returned %d (%v) for a scope that permits nothing", n, err)
			}
		})
	}
}

// TestIntegration_ScopedCallbackRootReads_UnownedRowsBelongToNoTenant pins the
// treatment of organization_id IS NULL — the one behaviour of this predicate
// that cannot be read off the Go source.
//
// These three roots are the ones 000033 says the column exists for: a drift run
// outlives its source, still holding its state_key, its plan summary and its
// callback_token. Migration 000034 made the column NOT NULL, so the constraint
// is relaxed for the length of this test and restored afterwards — the read
// layer's refusal to hand an unstamped row to a tenant is defence in depth, and
// defence in depth is exactly what has to keep working when the layer above it
// is absent (a database restored from a pre-000034 backup holds these rows).
func TestIntegration_ScopedCallbackRootReads_UnownedRowsBelongToNoTenant(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	seedSourceInOrg(t, db, orgAlpha, "alpha-unowned")

	for _, table := range []string{"drift_runs", "health_runs", "drift_records"} {
		if _, err := db.Exec(`ALTER TABLE ` + table + ` ALTER COLUMN organization_id DROP NOT NULL`); err != nil {
			t.Fatalf("relax NOT NULL on %s: %v", table, err)
		}
	}
	t.Cleanup(func() {
		// Restored even on failure: a later test in this package would otherwise
		// run against a schema this one quietly weakened.
		for _, table := range []string{"drift_runs", "health_runs", "drift_records"} {
			_, _ = db.Exec(`DELETE FROM ` + table + ` WHERE organization_id IS NULL`)
			_, _ = db.Exec(`ALTER TABLE ` + table + ` ALTER COLUMN organization_id SET NOT NULL`)
		}
	})

	var orphanRun, orphanHealth, orphanRecord string
	if err := db.QueryRow(
		`INSERT INTO drift_runs (status, callback_token, state_key, organization_id)
		 VALUES ('completed', 'orphan-tok', 'orphan.tfstate', NULL) RETURNING id::text`).Scan(&orphanRun); err != nil {
		t.Fatalf("seed unowned drift_run: %v", err)
	}
	if err := db.QueryRow(
		`INSERT INTO health_runs (status, callback_token, organization_id)
		 VALUES ('completed', 'orphan-tok', NULL) RETURNING id::text`).Scan(&orphanHealth); err != nil {
		t.Fatalf("seed unowned health_run: %v", err)
	}
	if err := db.QueryRow(
		`INSERT INTO drift_records (source_id, state_key, organization_id)
		 VALUES (NULL, 'orphan.tfstate', NULL) RETURNING id::text`).Scan(&orphanRecord); err != nil {
		t.Fatalf("seed unowned drift_record: %v", err)
	}

	drift := repositories.NewDriftRepository(db)
	health := repositories.NewHealthRepository(db)
	records := repositories.NewDriftRecordRepository(db)
	tenant := tenantscope.Scope{OrgIDs: []string{orgAlpha}}
	admin := tenantscope.Scope{PlatformAdmin: true}

	// Invisible to a tenant. `NULL = ANY(...)` must never be true: on these
	// tables NULL means "no tenant has been asserted", not "belongs to everyone",
	// and admitting these rows leaks whichever tenant owns the most of them.
	if got, err := drift.GetByIDInScope(ctx, orphanRun, tenant); err != nil || got != nil {
		t.Errorf("an unstamped drift_run was handed to a tenant: %+v %v", got, err)
	}
	if got, err := health.GetByIDInScope(ctx, orphanHealth, tenant); err != nil || got != nil {
		t.Errorf("an unstamped health_run was handed to a tenant: %+v %v", got, err)
	}
	if got, err := records.GetByIDInScope(ctx, orphanRecord, tenant); err != nil || got != nil {
		t.Errorf("an unstamped drift_record was handed to a tenant: %+v %v", got, err)
	}

	// ...and still reachable by the one principal that is deployment-wide, which
	// is what keeps such a row repairable rather than merely lost.
	for _, tc := range []struct {
		name string
		find func() (bool, error)
	}{
		{"drift_run", func() (bool, error) {
			got, err := drift.GetByIDInScope(ctx, orphanRun, admin)
			return got != nil, err
		}},
		{"health_run", func() (bool, error) {
			got, err := health.GetByIDInScope(ctx, orphanHealth, admin)
			return got != nil, err
		}},
		{"drift_record", func() (bool, error) {
			got, err := records.GetByIDInScope(ctx, orphanRecord, admin)
			return got != nil, err
		}},
	} {
		found, err := tc.find()
		if err != nil {
			t.Fatalf("platform-admin read of the unstamped %s: %v", tc.name, err)
		}
		if !found {
			t.Errorf("a platform admin could not see the unstamped %s. They are the only principal "+
				"who can, so this row would be invisible to everybody and unfixable through the API.", tc.name)
		}
	}
}

// ---------------------------------------------------------------------------
// Phase 4a: the coverage and summary readers (LatestPerStateInScope,
// LiveByStateInScope, CountsBySourceInScope, CountIncompleteInScope), which
// back GET /drift/coverage and GET /drift/summary.
//
// internal/db/repositories/callback_root_scope_test.go proves these bind the
// organization array as $1 -- against sqlmock, which matches SQL text and
// never evaluates a WHERE clause, so it can say the array was bound and it
// cannot say a row in another organization was refused. The four tests below
// answer that question against real PostgreSQL, each with the platform-admin
// control TestIntegration_ScopedCallbackRootReads_WithholdAnotherOrganization
// establishes above: the withholding is the predicate excluding a row, not a
// query that returns nothing to anybody.
// ---------------------------------------------------------------------------

// TestIntegration_LatestPerStateInScope_WithholdsMismatchedOrganization covers
// LatestPerStateInScope. Its own doc comment in callback_root_scope.go names
// the collision worth proving: the caller has already verified sourceID is in
// scope, so the organization predicate here is defense in depth against a run
// that names Alpha's source yet is itself stamped organization_id = Beta.
// Removing that predicate leaves a query keyed on source_id alone, which would
// return both state keys below instead of only the one that agrees.
func TestIntegration_LatestPerStateInScope_WithholdsMismatchedOrganization(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	srcA := seedSourceInOrg(t, db, orgAlpha, "alpha-coverage")
	seedDriftRunInOrg(t, db, orgAlpha, srcA, "envs/alpha.tfstate", "tok-a")
	// Names Alpha's source but is stamped Beta's organization -- the
	// disagreement the predicate exists to catch.
	seedDriftRunInOrg(t, db, orgBeta, srcA, "envs/beta.tfstate", "tok-b")

	drift := repositories.NewDriftRepository(db)
	scopeAlpha := tenantscope.Scope{OrgIDs: []string{orgAlpha}}
	scopeBeta := tenantscope.Scope{OrgIDs: []string{orgBeta}}
	admin := tenantscope.Scope{PlatformAdmin: true}

	latest, err := drift.LatestPerStateInScope(ctx, srcA, scopeAlpha)
	if err != nil {
		t.Fatalf("LatestPerStateInScope under Alpha: %v", err)
	}
	if _, present := latest["envs/beta.tfstate"]; present {
		t.Error("LatestPerStateInScope under Alpha's scope returned the mismatched Beta run")
	}
	if _, present := latest["envs/alpha.tfstate"]; !present {
		t.Error("LatestPerStateInScope under Alpha's scope withheld Alpha's own run")
	}

	// CONTROL: Beta's own scope reaches the row stamped as its own, on the same
	// source id -- without this the withholding above could just as well be a
	// reader that returns nothing to anybody.
	latestBeta, err := drift.LatestPerStateInScope(ctx, srcA, scopeBeta)
	if err != nil {
		t.Fatalf("LatestPerStateInScope under Beta: %v", err)
	}
	if _, present := latestBeta["envs/beta.tfstate"]; !present {
		t.Fatal("CONTROL FAILED: Beta's own scope cannot read the run stamped as its own")
	}

	// CONTROL: the platform admin sees both state keys for this source, so the
	// withholding above is the organization predicate at work, not the
	// source_id filter matching only one row.
	latestAdmin, err := drift.LatestPerStateInScope(ctx, srcA, admin)
	if err != nil {
		t.Fatalf("LatestPerStateInScope under admin: %v", err)
	}
	if len(latestAdmin) != 2 {
		t.Fatalf("CONTROL FAILED: platform admin saw %d state(s) for source %s, want 2", len(latestAdmin), srcA)
	}
}

// TestIntegration_LiveByStateInScope_WithholdsMismatchedOrganization is
// LiveByStateInScope's half of the same defense-in-depth claim, on
// drift_records rather than drift_runs.
func TestIntegration_LiveByStateInScope_WithholdsMismatchedOrganization(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	srcA := seedSourceInOrg(t, db, orgAlpha, "alpha-live-coverage")
	seedDriftRecordInOrg(t, db, orgAlpha, srcA, "envs/alpha.tfstate")
	// Names Alpha's source but is stamped Beta's organization.
	seedDriftRecordInOrg(t, db, orgBeta, srcA, "envs/beta.tfstate")

	records := repositories.NewDriftRecordRepository(db)
	scopeAlpha := tenantscope.Scope{OrgIDs: []string{orgAlpha}}
	scopeBeta := tenantscope.Scope{OrgIDs: []string{orgBeta}}
	admin := tenantscope.Scope{PlatformAdmin: true}

	live, err := records.LiveByStateInScope(ctx, srcA, scopeAlpha)
	if err != nil {
		t.Fatalf("LiveByStateInScope under Alpha: %v", err)
	}
	if _, present := live["envs/beta.tfstate"]; present {
		t.Error("LiveByStateInScope under Alpha's scope returned the mismatched Beta record")
	}
	if _, present := live["envs/alpha.tfstate"]; !present {
		t.Error("LiveByStateInScope under Alpha's scope withheld Alpha's own record")
	}

	// CONTROL: Beta's own scope reaches the record stamped as its own.
	liveBeta, err := records.LiveByStateInScope(ctx, srcA, scopeBeta)
	if err != nil {
		t.Fatalf("LiveByStateInScope under Beta: %v", err)
	}
	if _, present := liveBeta["envs/beta.tfstate"]; !present {
		t.Fatal("CONTROL FAILED: Beta's own scope cannot read the record stamped as its own")
	}

	// CONTROL: the platform admin sees both state keys for this source.
	liveAdmin, err := records.LiveByStateInScope(ctx, srcA, admin)
	if err != nil {
		t.Fatalf("LiveByStateInScope under admin: %v", err)
	}
	if len(liveAdmin) != 2 {
		t.Fatalf("CONTROL FAILED: platform admin saw %d record(s) for source %s, want 2", len(liveAdmin), srcA)
	}
}

// TestIntegration_CountsBySourceInScope_WithholdsAnotherOrganization covers
// the drift summary's per-source breakdown. Unlike the two tests above this
// one names no single source up front, so the collision is the ordinary one:
// two organizations each own a source named "prod" holding an identically
// shaped record (same state key, same severity, same status -- both written
// through seedDriftRecordInOrg).
func TestIntegration_CountsBySourceInScope_WithholdsAnotherOrganization(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	srcA := seedSourceInOrg(t, db, orgAlpha, "prod")
	srcB := seedSourceInOrg(t, db, orgBeta, "prod")
	seedDriftRecordInOrg(t, db, orgAlpha, srcA, "envs/prod.tfstate")
	seedDriftRecordInOrg(t, db, orgBeta, srcB, "envs/prod.tfstate")

	records := repositories.NewDriftRecordRepository(db)
	scopeAlpha := tenantscope.Scope{OrgIDs: []string{orgAlpha}}
	scopeBeta := tenantscope.Scope{OrgIDs: []string{orgBeta}}
	admin := tenantscope.Scope{PlatformAdmin: true}

	countsAlpha, err := records.CountsBySourceInScope(ctx, scopeAlpha)
	if err != nil {
		t.Fatalf("CountsBySourceInScope under Alpha: %v", err)
	}
	if len(countsAlpha) != 1 || countsAlpha[0].SourceID != srcA {
		t.Fatalf("CountsBySourceInScope under Alpha = %+v, want exactly source %s (\"prod\"); "+
			"Beta's identically named source must not be folded in", countsAlpha, srcA)
	}
	if countsAlpha[0].Open != 1 || countsAlpha[0].Critical != 1 {
		t.Fatalf("CountsBySourceInScope under Alpha = %+v, want 1 open and 1 critical, not the "+
			"deployment's total across both organizations", countsAlpha)
	}

	// CONTROL: Beta's own scope reaches its own identically-named source.
	countsBeta, err := records.CountsBySourceInScope(ctx, scopeBeta)
	if err != nil {
		t.Fatalf("CountsBySourceInScope under Beta: %v", err)
	}
	if len(countsBeta) != 1 || countsBeta[0].SourceID != srcB {
		t.Fatalf("CONTROL FAILED: CountsBySourceInScope under Beta = %+v, want exactly source %s", countsBeta, srcB)
	}

	// CONTROL: the platform admin sees both organizations' "prod" source.
	countsAdmin, err := records.CountsBySourceInScope(ctx, admin)
	if err != nil {
		t.Fatalf("CountsBySourceInScope under admin: %v", err)
	}
	if len(countsAdmin) != 2 {
		t.Fatalf("CONTROL FAILED: platform admin saw %d source(s), want 2 (same name, two organizations)", len(countsAdmin))
	}
}

// TestIntegration_CountIncompleteInScope_WithholdsAnotherOrganization covers
// the drift summary's incomplete_records field. seedDriftRecordInOrg always
// writes truncated=true and status='open' -- exactly what this reader counts
// -- so the collision worth seeding is the identical state key and source
// shape, not the incompleteness marker itself.
func TestIntegration_CountIncompleteInScope_WithholdsAnotherOrganization(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	srcA := seedSourceInOrg(t, db, orgAlpha, "alpha-incomplete")
	srcB := seedSourceInOrg(t, db, orgBeta, "beta-incomplete")
	seedDriftRecordInOrg(t, db, orgAlpha, srcA, "envs/prod.tfstate")
	seedDriftRecordInOrg(t, db, orgBeta, srcB, "envs/prod.tfstate")

	records := repositories.NewDriftRecordRepository(db)
	scopeAlpha := tenantscope.Scope{OrgIDs: []string{orgAlpha}}
	scopeBeta := tenantscope.Scope{OrgIDs: []string{orgBeta}}
	admin := tenantscope.Scope{PlatformAdmin: true}

	nAlpha, err := records.CountIncompleteInScope(ctx, scopeAlpha)
	if err != nil {
		t.Fatalf("CountIncompleteInScope under Alpha: %v", err)
	}
	if nAlpha != 1 {
		t.Fatalf("CountIncompleteInScope under Alpha = %d, want 1 -- not the deployment's total of 2", nAlpha)
	}

	// CONTROL: Beta counts its own.
	nBeta, err := records.CountIncompleteInScope(ctx, scopeBeta)
	if err != nil {
		t.Fatalf("CountIncompleteInScope under Beta: %v", err)
	}
	if nBeta != 1 {
		t.Fatalf("CONTROL FAILED: CountIncompleteInScope under Beta = %d, want 1", nBeta)
	}

	// CONTROL: the platform admin sees the deployment's total.
	nAdmin, err := records.CountIncompleteInScope(ctx, admin)
	if err != nil {
		t.Fatalf("CountIncompleteInScope under admin: %v", err)
	}
	if nAdmin != 2 {
		t.Fatalf("CONTROL FAILED: platform admin counted %d incomplete record(s), want 2", nAdmin)
	}
}

// ---------------------------------------------------------------------------
// The machine-callback half: what a run's own authority may and may not touch.
// ---------------------------------------------------------------------------

// TestIntegration_CallbackAuthority_CannotReachAnotherOrganizationsRows is the
// guard this increment exists for.
//
// The scenario is concrete. A CI job holds a legitimate callback token for a
// drift run in organization Alpha. Run ids and record ids are not secrets — the
// list endpoints hand them out — so the question is what that credential can be
// pointed at. Unscoped, the answer was "every row in the deployment": the
// callback's statements addressed rows by id, by (source_id, state_key), and by
// the source named on the run, none of which mentions an organization.
//
// Every refusal below has its owner-side control in the same subtest.
func TestIntegration_CallbackAuthority_CannotReachAnotherOrganizationsRows(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	srcA := seedSourceInOrg(t, db, orgAlpha, "alpha-callback")
	srcB := seedSourceInOrg(t, db, orgBeta, "beta-callback")
	runA := seedDriftRunInOrg(t, db, orgAlpha, srcA, "alpha.tfstate", "tok-a")
	runB := seedDriftRunInOrg(t, db, orgBeta, srcB, "beta.tfstate", "tok-b")
	healthA := seedHealthRunInOrg(t, db, orgAlpha, "htok-a")
	healthB := seedHealthRunInOrg(t, db, orgBeta, "htok-b")
	recB := seedDriftRecordInOrg(t, db, orgBeta, srcB, "beta.tfstate")

	drift := repositories.NewDriftRepository(db)
	health := repositories.NewHealthRepository(db)
	records := repositories.NewDriftRecordRepository(db)

	// The authority the Alpha callback derives, built by the same constructor the
	// handler calls.
	authA := callbackScope(t, orgAlpha, "drift_runs", runA)
	authB := callbackScope(t, orgBeta, "drift_runs", runB)
	healthAuthA := callbackScope(t, orgAlpha, "health_runs", healthA)
	healthAuthB := callbackScope(t, orgBeta, "health_runs", healthB)

	t.Run("it cannot read Beta's drift record", func(t *testing.T) {
		if got, err := records.GetByIDInScope(ctx, recB, authA); err != nil || got != nil {
			t.Errorf("Alpha's callback read Beta's drift record %s: %+v %v", recB, got, err)
		}
		if got, err := records.GetByIDInScope(ctx, recB, authB); err != nil || got == nil {
			t.Errorf("CONTROL FAILED: Beta's own callback cannot read Beta's record %s (%v). The "+
				"refusal above is then not evidence of scoping.", recB, err)
		}
	})

	t.Run("it cannot acknowledge Beta's drift record", func(t *testing.T) {
		// An acknowledgement is the statement that a human has SEEN a finding, so
		// this is a cross-organization silencing, not a disclosure.
		got, err := records.AcknowledgeInScope(ctx, recB, "alpha-ci", "not mine", authA)
		if err != nil || got != nil {
			t.Errorf("Alpha's callback acknowledged Beta's record %s: %+v %v", recB, got, err)
		}
		if status := recordStatus(t, db, recB); status != "open" {
			t.Errorf("Beta's record is now %q; Alpha's authority changed it", status)
		}
		got, err = records.AcknowledgeInScope(ctx, recB, "beta-ci", "mine", authB)
		if err != nil || got == nil {
			t.Fatalf("CONTROL FAILED: Beta's own authority could not acknowledge Beta's record: %+v %v", got, err)
		}
		if status := recordStatus(t, db, recB); status != "acknowledged" {
			t.Fatalf("CONTROL FAILED: Beta's record is %q after its owner acknowledged it", status)
		}
	})

	t.Run("it cannot resolve Beta's drift record by id", func(t *testing.T) {
		// The manual-resolve path, which the request side serves at
		// POST /drift/records/:id/resolve and which the same predicate protects.
		reopen(t, db, recB)
		got, err := records.ResolveInScope(ctx, recB, authA)
		if err != nil || got != nil {
			t.Errorf("Alpha's authority resolved Beta's record %s by id: %+v %v", recB, got, err)
		}
		if status := recordStatus(t, db, recB); status == "resolved" {
			t.Error("Beta's drift record was closed by id under Alpha's authority")
		}
		got, err = records.ResolveInScope(ctx, recB, authB)
		if err != nil || got == nil {
			t.Fatalf("CONTROL FAILED: Beta's own authority could not resolve its record by id: %+v %v", got, err)
		}
	})

	t.Run("it cannot count Beta's drift records", func(t *testing.T) {
		// The filtered total behind "showing N of M". An unscoped one reports how
		// many findings the rest of the deployment is carrying.
		n, err := records.CountRecordsInScope(ctx, nil, "", "", nil, nil, authA)
		if err != nil {
			t.Fatalf("CountRecordsInScope under Alpha: %v", err)
		}
		if n != 0 {
			t.Errorf("Alpha's callback counted %d drift records; Alpha owns none here", n)
		}
		n, err = records.CountRecordsInScope(ctx, nil, "", "", nil, nil, authB)
		if err != nil || n == 0 {
			t.Fatalf("CONTROL FAILED: Beta's own authority counted %d of its own records (%v)", n, err)
		}
	})

	t.Run("it cannot resolve Beta's live drift record", func(t *testing.T) {
		// The destructive half: a clean result posted with Alpha's token closing a
		// live finding on Beta's infrastructure. This is what
		// recordDriftOutcome's clean branch does, keyed on (source_id, state_key)
		// alone before this increment.
		reopen(t, db, recB)
		ok, err := records.ResolveCleanInScope(ctx, srcB, "beta.tfstate", authA)
		if err != nil || ok {
			t.Errorf("Alpha's callback resolved Beta's live record (%v, %v)", ok, err)
		}
		if status := recordStatus(t, db, recB); status == "resolved" {
			t.Error("Beta's live drift record was closed under Alpha's authority")
		}
		ok, err = records.ResolveCleanInScope(ctx, srcB, "beta.tfstate", authB)
		if err != nil || !ok {
			t.Fatalf("CONTROL FAILED: Beta's own authority could not resolve Beta's record (%v, %v)", ok, err)
		}
	})

	t.Run("it cannot write a detection into Beta's ledger", func(t *testing.T) {
		det := &repositories.Detection{
			SourceID: srcB, StateKey: "beta-injected.tfstate", Origin: "run", Added: 9,
		}
		rec, err := records.UpsertDetectionInScope(ctx, det, authA)
		if err == nil || rec != nil {
			t.Errorf("Alpha's callback wrote a drift record against Beta's source: %+v %v", rec, err)
		}
		if n := recordCount(t, db, srcB, "beta-injected.tfstate"); n != 0 {
			t.Errorf("%d drift record(s) landed in Beta's ledger under Alpha's authority", n)
		}
		rec, err = records.UpsertDetectionInScope(ctx, det, authB)
		if err != nil || rec == nil {
			t.Fatalf("CONTROL FAILED: Beta's own authority could not record a detection on Beta's "+
				"source: %+v %v", rec, err)
		}
		if rec.OrganizationID != orgBeta {
			t.Errorf("the detection landed in organization %q, want Beta", rec.OrganizationID)
		}
	})

	t.Run("it cannot rewrite Beta's module provenance", func(t *testing.T) {
		// The write the survey of this path nearly missed. captureModuleRefs is
		// best-effort provenance, so it never fails a callback and never appears
		// in an error path -- and it DELETEs before it INSERTs, keyed on the
		// source the run names. Unscoped, Alpha's callback would not add a wrong
		// row to Beta's ledger, it would destroy Beta's right ones.
		refs := repositories.NewStateModuleRefRepository(db)
		seedModuleRef(t, db, srcB, "beta.tfstate", "acme/vpc/aws", "5.3.0")

		err := refs.ReplaceForStateInScope(ctx, srcB, "beta.tfstate", nil, authA)
		if err == nil {
			t.Error("Alpha's callback rewrote the module provenance of Beta's state file")
		}
		if n := moduleRefCount(t, db, srcB, "beta.tfstate"); n != 1 {
			t.Errorf("Beta has %d module ref(s) left after Alpha's callback; it seeded 1", n)
		}
		// CONTROL: Beta's own authority must still be able to replace them, or
		// the refusal above is a writer that writes for nobody.
		version := "1.0.0"
		if err := refs.ReplaceForStateInScope(ctx, srcB, "beta.tfstate", []repositories.StateModuleRef{
			{ModuleSource: "acme/rds/aws", ModuleVersion: &version, RegistryHost: "registry.terraform.io"},
		}, authB); err != nil {
			t.Fatalf("CONTROL FAILED: Beta's own authority could not replace its module refs: %v", err)
		}
		if n := moduleRefCount(t, db, srcB, "beta.tfstate"); n != 1 {
			t.Fatalf("CONTROL FAILED: Beta has %d module ref(s) after replacing with one", n)
		}
	})

	t.Run("it cannot read or write Beta's health run", func(t *testing.T) {
		if got, err := health.GetByIDInScope(ctx, healthB, healthAuthA); err != nil || got != nil {
			t.Errorf("Alpha's callback read Beta's health run %s: %+v %v", healthB, got, err)
		}
		if err := health.UpdateResultInScope(ctx, healthB, "failed", false, false, false, nil, "poisoned by alpha", healthAuthA); err != nil {
			t.Fatalf("UpdateResultInScope under Alpha: %v", err)
		}
		if detail := healthDetail(t, db, healthB); detail == "poisoned by alpha" {
			t.Error("Alpha's callback overwrote Beta's health-run result")
		}
		// CONTROL: the same statement under Beta's own authority must land.
		if err := health.UpdateResultInScope(ctx, healthB, "failed", false, false, false, nil, "beta's own result", healthAuthB); err != nil {
			t.Fatalf("CONTROL: UpdateResultInScope under Beta: %v", err)
		}
		if detail := healthDetail(t, db, healthB); detail != "beta's own result" {
			t.Fatalf("CONTROL FAILED: Beta's own authority did not write its health run (detail = %q); "+
				"the refusal above is then not evidence of scoping", detail)
		}
		if got, err := health.GetByIDInScope(ctx, healthB, healthAuthB); err != nil || got == nil {
			t.Errorf("CONTROL FAILED: Beta's own callback cannot read Beta's health run: %v", err)
		}
	})

	t.Run("it cannot read or write Beta's drift run", func(t *testing.T) {
		if got, err := drift.GetByIDInScope(ctx, runB, authA); err != nil || got != nil {
			t.Errorf("Alpha's callback read Beta's drift run %s: %+v %v", runB, got, err)
		}
		if err := drift.UpdateResultInScope(ctx, runB, "failed", 0, 0, 0, false, nil, "poisoned by alpha",
			repositories.Completeness{}, repositories.InfraDrift{}, authA); err != nil {
			t.Fatalf("UpdateResultInScope under Alpha: %v", err)
		}
		if detail := driftDetail(t, db, runB); detail == "poisoned by alpha" {
			t.Error("Alpha's callback overwrote Beta's drift-run result")
		}
		if err := drift.UpdateResultInScope(ctx, runB, "failed", 0, 0, 0, false, nil, "beta's own result",
			repositories.Completeness{}, repositories.InfraDrift{}, authB); err != nil {
			t.Fatalf("CONTROL: UpdateResultInScope under Beta: %v", err)
		}
		if detail := driftDetail(t, db, runB); detail != "beta's own result" {
			t.Fatalf("CONTROL FAILED: Beta's own authority did not write its drift run (detail = %q)", detail)
		}
	})
}

// TestIntegration_CallbackAuthority_OfAnUnstampedRunReachesNothing is the other
// end of the derivation, against the database.
//
// A run restored from a pre-000034 backup has no organization. The derivation
// refuses to build an authority from it (asserted directly in
// internal/api/callback_authority_test.go); this pins what the resulting empty
// scope DOES against real SQL — nothing, in either direction — so that a caller
// which ignored the refusal still reaches no row.
func TestIntegration_CallbackAuthority_OfAnUnstampedRunReachesNothing(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	srcA := seedSourceInOrg(t, db, orgAlpha, "alpha-unstamped-auth")
	recA := seedDriftRecordInOrg(t, db, orgAlpha, srcA, "alpha.tfstate")

	if _, err := SystemActingIn("", "drift_runs", "d-unstamped"); err == nil {
		t.Fatal("SystemActingIn built an authority from a run with no organization; the callback " +
			"would then act under whatever that authority permits")
	}

	// What a caller that ignored the refusal would be holding.
	var underived SystemScope
	empty := underived.Scope()
	records := repositories.NewDriftRecordRepository(db)

	if got, err := records.GetByIDInScope(ctx, recA, empty); err != nil || got != nil {
		t.Errorf("an underived authority read a drift record: %+v %v", got, err)
	}
	if _, err := records.UpsertDetectionInScope(ctx, &repositories.Detection{
		SourceID: srcA, StateKey: "alpha.tfstate", Origin: "run", Added: 1,
	}, empty); err == nil {
		t.Error("an underived authority wrote a detection")
	}
	if _, err := records.ResolveCleanInScope(ctx, srcA, "alpha.tfstate", empty); err == nil {
		t.Error("an underived authority resolved a record")
	}

	// CONTROL: the same row is reachable under a real derived authority, so the
	// refusals above are about the authority and not about the fixture.
	real := callbackScope(t, orgAlpha, "drift_runs", "00000000-0000-4000-8000-000000000001")
	if got, err := records.GetByIDInScope(ctx, recA, real); err != nil || got == nil {
		t.Fatalf("CONTROL FAILED: a derived Alpha authority could not read Alpha's record: %v", err)
	}
}

func recordStatus(t *testing.T, db *sql.DB, id string) string {
	t.Helper()
	var status string
	if err := db.QueryRow(`SELECT status FROM drift_records WHERE id = $1`, id).Scan(&status); err != nil {
		t.Fatalf("read drift_record status: %v", err)
	}
	return status
}

func recordCount(t *testing.T, db *sql.DB, sourceID, stateKey string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM drift_records WHERE source_id = $1 AND state_key = $2`, sourceID, stateKey).Scan(&n); err != nil {
		t.Fatalf("count drift_records: %v", err)
	}
	return n
}

func reopen(t *testing.T, db *sql.DB, id string) {
	t.Helper()
	if _, err := db.Exec(`UPDATE drift_records SET status='open', resolved_at=NULL WHERE id = $1`, id); err != nil {
		t.Fatalf("reopen drift_record: %v", err)
	}
}

func healthDetail(t *testing.T, db *sql.DB, id string) string {
	t.Helper()
	var detail sql.NullString
	if err := db.QueryRow(`SELECT detail FROM health_runs WHERE id = $1`, id).Scan(&detail); err != nil {
		t.Fatalf("read health_run detail: %v", err)
	}
	return detail.String
}

func driftDetail(t *testing.T, db *sql.DB, id string) string {
	t.Helper()
	var detail sql.NullString
	if err := db.QueryRow(`SELECT detail FROM drift_runs WHERE id = $1`, id).Scan(&detail); err != nil {
		t.Fatalf("read drift_run detail: %v", err)
	}
	return detail.String
}

func seedModuleRef(t *testing.T, db *sql.DB, sourceID, stateKey, moduleSource, version string) {
	t.Helper()
	if _, err := db.Exec(
		`INSERT INTO state_module_refs (source_id, state_key, module_source, module_version, registry_host)
		 VALUES ($1, $2, $3, $4, 'registry.terraform.io')`,
		sourceID, stateKey, moduleSource, version); err != nil {
		t.Fatalf("seed state_module_ref: %v", err)
	}
}

func moduleRefCount(t *testing.T, db *sql.DB, sourceID, stateKey string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM state_module_refs WHERE source_id = $1 AND state_key = $2`,
		sourceID, stateKey).Scan(&n); err != nil {
		t.Fatalf("count state_module_refs: %v", err)
	}
	return n
}
