//go:build integration

package repositories_test

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
	_ "github.com/jackc/pgx/v5/stdlib"

	appdb "github.com/terraform-state-manager/terraform-state-manager/internal/db"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
	"github.com/terraform-state-manager/terraform-state-manager/internal/tenantscope"
)

// AGAINST A REAL POSTGRESQL, because the thing under test IS a join.
//
// These tables carry no organization_id -- 000033 argues, correctly, that the
// parent's organization IS the row's and a duplicated column would be the copy
// that goes stale. The consequence is that the tenant predicate here is not a
// WHERE on a column a mock could be told about; it is an ownership edge that only
// a database can walk. sqlmock returns whatever its fixture declares, so under it
// a scoped reader and an unscoped one are indistinguishable.

const scopeTestDB = "tsm_analysis_scope_test"

const (
	orgA = "aaaaaaaa-0000-4000-8000-00000000000a"
	orgB = "bbbbbbbb-0000-4000-8000-00000000000b"
)

func newScopeDB(t *testing.T) *sql.DB {
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
		`DROP DATABASE IF EXISTS ` + pgx.Identifier{scopeTestDB}.Sanitize() + ` WITH (FORCE)`,
		`CREATE DATABASE ` + pgx.Identifier{scopeTestDB}.Sanitize(),
	} {
		if _, err := admin.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	parsed.Path = "/" + scopeTestDB
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

// seedTwoTenants gives organization A two analyzed states and organization B
// one, so every assertion below can distinguish "scoped correctly" from both
// "returned everything" and "returned nothing".
func seedTwoTenants(t *testing.T, db *sql.DB) {
	t.Helper()
	stmts := []string{
		`INSERT INTO state_sources (id, name, type, config, organization_id) VALUES
		   ('11110000-0000-4000-8000-000000000001','a-one','local','{}','` + orgA + `'),
		   ('11110000-0000-4000-8000-000000000002','a-two','local','{}','` + orgA + `'),
		   ('22220000-0000-4000-8000-000000000001','b-one','local','{}','` + orgB + `')`,
		`INSERT INTO state_analyses (source_id, state_key, terraform_version, rum, managed_resources, providers, resource_types) VALUES
		   ('11110000-0000-4000-8000-000000000001','k1','1.9.5',10,5,'{"aws":2}','{"aws_instance":2}'),
		   ('11110000-0000-4000-8000-000000000002','k2','1.9.5',20,7,'{"aws":3}','{"aws_instance":3}'),
		   ('22220000-0000-4000-8000-000000000001','k3','1.6.6',99,50,'{"gcp":9}','{"google_bucket":9}')`,
		`INSERT INTO source_sync_status (source_id, last_sync_at, states_listed, read_errors) VALUES
		   ('11110000-0000-4000-8000-000000000001', now(), 1, 0),
		   ('22220000-0000-4000-8000-000000000001', now(), 1, 0)`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("seed: %v\n%s", err, s)
		}
	}
}

func scopeA() tenantscope.Scope { return tenantscope.Scope{OrgIDs: []string{orgA}} }

func TestIntegration_ScopedAnalysisReaders_SeeOnlyTheirOwnTenant(t *testing.T) {
	db := newScopeDB(t)
	seedTwoTenants(t, db)
	r := repositories.NewStateAnalysisRepository(db)
	ctx := context.Background()

	// Totals: A's two states, not B's third, and B's RUM of 99 must not appear.
	totals, err := r.TotalsInScope(ctx, scopeA())
	if err != nil {
		t.Fatalf("TotalsInScope: %v", err)
	}
	if totals.States != 2 {
		t.Errorf("States = %d, want 2 (3 means the fleet, 0 means the join is wrong)", totals.States)
	}
	if totals.RUM != 30 {
		t.Errorf("RUM = %d, want 30 -- 129 would include the other tenant's 99", totals.RUM)
	}

	// Provider counts: A uses aws only; B's gcp must not leak into the histogram.
	providers, err := r.ProviderCountsInScope(ctx, scopeA())
	if err != nil {
		t.Fatalf("ProviderCountsInScope: %v", err)
	}
	if _, leaked := providers["gcp"]; leaked {
		t.Errorf("provider histogram leaked another tenant's provider: %v", providers)
	}
	if providers["aws"] != 5 {
		t.Errorf("aws = %d, want 5", providers["aws"])
	}

	types, err := r.ResourceTypeCountsInScope(ctx, scopeA())
	if err != nil {
		t.Fatalf("ResourceTypeCountsInScope: %v", err)
	}
	if _, leaked := types["google_bucket"]; leaked {
		t.Errorf("resource-type histogram leaked another tenant's type: %v", types)
	}

	// Version counts: A is all 1.9.5; B's 1.6.6 must be absent entirely, since
	// its presence would disclose that some other tenant runs that version.
	versions, err := r.VersionCountsInScope(ctx, scopeA())
	if err != nil {
		t.Fatalf("VersionCountsInScope: %v", err)
	}
	if _, leaked := versions["1.6.6"]; leaked {
		t.Errorf("version histogram leaked another tenant's version: %v", versions)
	}

	rows, err := r.StateVersionsInScope(ctx, scopeA())
	if err != nil {
		t.Fatalf("StateVersionsInScope: %v", err)
	}
	if len(rows) != 2 {
		t.Errorf("StateVersionsInScope returned %d rows, want 2", len(rows))
	}

	// The window aggregate must summarise the SCOPED set: a fleet-wide total
	// beside a scoped page discloses how many states exist elsewhere.
	states, total, err := r.StatesByVersionExactInScope(ctx, scopeA(), "1.9.5", 100)
	if err != nil {
		t.Fatalf("StatesByVersionExactInScope: %v", err)
	}
	if len(states) != 2 || total != 2 {
		t.Errorf("StatesByVersionExactInScope = %d rows, total %d; want 2 and 2", len(states), total)
	}

	filtered, err := r.FilterStatesInScope(ctx, scopeA(), repositories.StateQueryFilter{})
	if err != nil {
		t.Fatalf("FilterStatesInScope: %v", err)
	}
	if len(filtered) != 2 {
		t.Errorf("FilterStatesInScope returned %d rows, want 2", len(filtered))
	}

	preview, agg, err := r.PreviewStatesWithTotalsInScope(ctx, scopeA(), repositories.StateQueryFilter{}, 100)
	if err != nil {
		t.Fatalf("PreviewStatesWithTotalsInScope: %v", err)
	}
	if len(preview) != 2 || agg.Matched != 2 || agg.RUM != 30 {
		t.Errorf("preview = %d rows, matched %d, rum %d; want 2, 2, 30", len(preview), agg.Matched, agg.RUM)
	}

	sync, err := r.SyncStatusesInScope(ctx, scopeA())
	if err != nil {
		t.Fatalf("SyncStatusesInScope: %v", err)
	}
	if len(sync) != 1 {
		t.Errorf("SyncStatusesInScope returned %d rows, want 1", len(sync))
	}
	if _, leaked := sync["22220000-0000-4000-8000-000000000001"]; leaked {
		t.Error("sync statuses leaked another tenant's source")
	}
}

// TestIntegration_ScopedAnalysisReaders_EmptyScopeReadsNothing is the fail-closed
// direction: a caller whose tenancy could not be established selects no rows
// rather than every row.
func TestIntegration_ScopedAnalysisReaders_EmptyScopeReadsNothing(t *testing.T) {
	db := newScopeDB(t)
	seedTwoTenants(t, db)
	r := repositories.NewStateAnalysisRepository(db)
	ctx := context.Background()
	empty := tenantscope.Scope{}

	totals, err := r.TotalsInScope(ctx, empty)
	if err != nil {
		t.Fatalf("TotalsInScope: %v", err)
	}
	if totals.States != 0 || totals.RUM != 0 {
		t.Errorf("an empty scope read %d states / %d RUM; it must read nothing", totals.States, totals.RUM)
	}
	if rows, err := r.StateVersionsInScope(ctx, empty); err != nil || len(rows) != 0 {
		t.Errorf("an empty scope read %d version rows (err %v); it must read nothing", len(rows), err)
	}
	if s, err := r.SyncStatusesInScope(ctx, empty); err != nil || len(s) != 0 {
		t.Errorf("an empty scope read %d sync statuses (err %v); it must read nothing", len(s), err)
	}
}

// TestIntegration_ScopedAnalysisReaders_PlatformAdminSeesEverything. Without
// this, every assertion above is satisfied by a reader that returns nothing.
func TestIntegration_ScopedAnalysisReaders_PlatformAdminSeesEverything(t *testing.T) {
	db := newScopeDB(t)
	seedTwoTenants(t, db)
	r := repositories.NewStateAnalysisRepository(db)
	ctx := context.Background()
	admin := tenantscope.Scope{PlatformAdmin: true}

	totals, err := r.TotalsInScope(ctx, admin)
	if err != nil {
		t.Fatalf("TotalsInScope: %v", err)
	}
	if totals.States != 3 {
		t.Errorf("platform admin saw %d states, want all 3", totals.States)
	}
}

// TestIntegration_ScopedAnalysisReaders_UnstampedSourceIsInvisible. A source
// whose organization_id is still NULL -- written by a replica on a previous
// build, before the backfill -- must not be visible to a tenant. The predicate
// is `= ANY(...)`, and `NULL = ANY(...)` is NULL rather than true, so this is a
// property of the SQL that only a real server can demonstrate.
func TestIntegration_ScopedAnalysisReaders_UnstampedSourceIsInvisible(t *testing.T) {
	db := newScopeDB(t)
	seedTwoTenants(t, db)
	// Phase 4 (000034) makes organization_id NOT NULL, so this state has to be
	// created deliberately. It is still worth testing: the join's NULL-exclusion
	// is DEFENCE IN DEPTH, and the state is reachable in practice by restoring a
	// backup taken before that migration. A predicate that stopped excluding NULL
	// would hand every such row to whichever tenant asked.
	if _, err := db.Exec(`ALTER TABLE state_sources ALTER COLUMN organization_id DROP NOT NULL`); err != nil {
		t.Fatalf("relax NOT NULL: %v", err)
	}
	if _, err := db.Exec(
		`UPDATE state_sources SET organization_id = NULL WHERE id = '11110000-0000-4000-8000-000000000001'`); err != nil {
		t.Fatalf("unstamp: %v", err)
	}
	r := repositories.NewStateAnalysisRepository(db)

	totals, err := r.TotalsInScope(context.Background(), scopeA())
	if err != nil {
		t.Fatalf("TotalsInScope: %v", err)
	}
	if totals.States != 1 {
		t.Errorf("States = %d, want 1: the unstamped source's state must be invisible", totals.States)
	}
}
