package repositories

import (
	"database/sql"
	"errors"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"

	"github.com/terraform-state-manager/terraform-state-manager/internal/tenantscope"
)

// ---------------------------------------------------------------------------
// The callback roots — organization-scoped reads and writes (#393 Phase 3).
//
// THESE COVER THE SHAPE OF THE STATEMENT AND NOTHING MORE, and the distinction
// matters enough to state at the top of the file. sqlmock matches SQL by regex
// and returns the rows the test handed it; it never evaluates a WHERE clause.
// So it can say "the organization array was bound as $1" and it CANNOT say "a
// row in another organization was not served" — those are different claims, and
// the second one is answered against a real PostgreSQL in
// internal/tenancy/callback_roots_integration_test.go.
//
// That is not a caution copied from somewhere. It was proved on the previous
// increment: replacing a derived authority with a permissive platform-admin
// scope, which makes every GetByIDInScope take its bypass branch and serve any
// organization's row, compiled and left the whole sqlmock dispatch suite green.
//
// MUTATION-VERIFIED. Each case below was run against a deliberately broken
// repository and observed to fail; the table is in the commit message.
// ---------------------------------------------------------------------------

// THE FAIL-CLOSED CASE, asserted as "no statement reached the database" rather
// than as "no rows came back". A reader that issued the query and happened to
// match nothing satisfies the second while remaining one predicate edit away
// from returning the whole table.
func TestCallbackRoots_ScopedReadsOnAnEmptyScopeTouchNothing(t *testing.T) {
	t.Run("drift_runs", func(t *testing.T) {
		db, mock := newMock(t)
		r := NewDriftRepository(db)
		if out, err := r.ListInScope(ctx, 0, 0, "", tenantscope.Scope{}); err != nil || len(out) != 0 {
			t.Errorf("ListInScope on an empty scope = %v, %v; want no runs", out, err)
		}
		if n, err := r.CountRunsInScope(ctx, "", tenantscope.Scope{}); err != nil || n != 0 {
			t.Errorf("CountRunsInScope on an empty scope = %d, %v; want 0", n, err)
		}
		if got, err := r.GetByIDInScope(ctx, "d1", tenantscope.Scope{}); err != nil || got != nil {
			t.Errorf("GetByIDInScope on an empty scope = %+v, %v; want nothing", got, err)
		}
		if err := r.UpdateResultInScope(ctx, "d1", "completed", 0, 0, 0, false, nil, "", Completeness{}, tenantscope.Scope{}); !errors.Is(err, ErrNotInScope) {
			t.Errorf("UpdateResultInScope on an empty scope = %v; want ErrNotInScope", err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Error(err)
		}
	})

	t.Run("health_runs", func(t *testing.T) {
		db, mock := newMock(t)
		r := NewHealthRepository(db)
		if out, err := r.ListInScope(ctx, 0, 0, "", tenantscope.Scope{}); err != nil || len(out) != 0 {
			t.Errorf("ListInScope on an empty scope = %v, %v; want no runs", out, err)
		}
		if n, err := r.CountRunsInScope(ctx, "", tenantscope.Scope{}); err != nil || n != 0 {
			t.Errorf("CountRunsInScope on an empty scope = %d, %v; want 0", n, err)
		}
		if got, err := r.GetByIDInScope(ctx, "h1", tenantscope.Scope{}); err != nil || got != nil {
			t.Errorf("GetByIDInScope on an empty scope = %+v, %v; want nothing", got, err)
		}
		if err := r.UpdateResultInScope(ctx, "h1", "completed", true, true, true, nil, "", tenantscope.Scope{}); !errors.Is(err, ErrNotInScope) {
			t.Errorf("UpdateResultInScope on an empty scope = %v; want ErrNotInScope", err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Error(err)
		}
	})

	t.Run("drift_records", func(t *testing.T) {
		db, mock := newMock(t)
		r := NewDriftRecordRepository(db)
		if out, err := r.ListInScope(ctx, nil, "", "", 0, 0, nil, nil, tenantscope.Scope{}); err != nil || len(out) != 0 {
			t.Errorf("ListInScope on an empty scope = %v, %v; want no records", out, err)
		}
		if n, err := r.CountRecordsInScope(ctx, nil, "", "", nil, nil, tenantscope.Scope{}); err != nil || n != 0 {
			t.Errorf("CountRecordsInScope on an empty scope = %d, %v; want 0", n, err)
		}
		if m, err := r.CountsByStatusInScope(ctx, tenantscope.Scope{}); err != nil || len(m) != 0 {
			t.Errorf("CountsByStatusInScope on an empty scope = %v, %v; want an empty tally", m, err)
		}
		if got, err := r.GetByIDInScope(ctx, "r1", tenantscope.Scope{}); err != nil || got != nil {
			t.Errorf("GetByIDInScope on an empty scope = %+v, %v; want nothing", got, err)
		}
		if _, err := r.AcknowledgeInScope(ctx, "r1", "alice", "", tenantscope.Scope{}); !errors.Is(err, ErrNotInScope) {
			t.Errorf("AcknowledgeInScope on an empty scope = %v; want ErrNotInScope", err)
		}
		if _, err := r.ResolveInScope(ctx, "r1", tenantscope.Scope{}); !errors.Is(err, ErrNotInScope) {
			t.Errorf("ResolveInScope on an empty scope = %v; want ErrNotInScope", err)
		}
		if _, err := r.ResolveCleanInScope(ctx, "s1", "k", tenantscope.Scope{}); !errors.Is(err, ErrNotInScope) {
			t.Errorf("ResolveCleanInScope on an empty scope = %v; want ErrNotInScope", err)
		}
		if _, err := r.UpsertDetectionInScope(ctx, &Detection{SourceID: "s1", StateKey: "k"}, tenantscope.Scope{}); !errors.Is(err, ErrNotInScope) {
			t.Errorf("UpsertDetectionInScope on an empty scope = %v; want ErrNotInScope", err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Error(err)
		}
	})
}

// The organization array is $1 and the id is $2 on every by-id read, so the two
// readers on each root share one predicate string. If that order is ever
// reversed these expectations say so — and a by-id read bound to the wrong
// parameter is somebody else's row.
func TestCallbackRoots_ScopedReads_BindTheOrganization(t *testing.T) {
	scope := tenantscope.Scope{OrgIDs: []string{scopeOrgA}}

	t.Run("drift_runs", func(t *testing.T) {
		db, mock := newMock(t)
		r := NewDriftRepository(db)

		mock.ExpectQuery("FROM drift_runs WHERE organization_id = ANY").
			WithArgs([]string{scopeOrgA}, 50, 0).WillReturnRows(driftRow("secret"))
		out, err := r.ListInScope(ctx, 0, 0, "", scope)
		if err != nil || len(out) != 1 {
			t.Fatalf("ListInScope: %v %+v", err, out)
		}
		if out[0].CallbackToken != "" {
			t.Error("ListInScope returned a callback token; the scoped list must strip it exactly as List does")
		}

		mock.ExpectQuery(`SELECT COUNT\(\*\) FROM drift_runs WHERE organization_id = ANY`).
			WithArgs([]string{scopeOrgA}).WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(3))
		if n, err := r.CountRunsInScope(ctx, "", scope); err != nil || n != 3 {
			t.Fatalf("CountRunsInScope = %d, %v", n, err)
		}

		mock.ExpectQuery("FROM drift_runs WHERE organization_id = ANY").
			WithArgs([]string{scopeOrgA}, "d1").WillReturnRows(driftRow("secret"))
		if got, err := r.GetByIDInScope(ctx, "d1", scope); err != nil || got == nil || got.ID != "d1" {
			t.Fatalf("GetByIDInScope: %v %+v", err, got)
		}

		// A run in another organization reads EXACTLY as one that does not exist.
		mock.ExpectQuery("FROM drift_runs WHERE organization_id = ANY").
			WithArgs([]string{scopeOrgA}, "elsewhere").WillReturnError(sql.ErrNoRows)
		if got, err := r.GetByIDInScope(ctx, "elsewhere", scope); err != nil || got != nil {
			t.Errorf("a run outside the scope must be (nil, nil), got %+v %v", got, err)
		}

		// The write side binds the organization LAST, after the thirteen value
		// arguments.
		mock.ExpectExec("UPDATE drift_runs SET status.+organization_id = ANY").
			WithArgs("d1", "completed", 1, 2, 3, true, nil, "detail", false, 0, 0, false, false, []string{scopeOrgA}).
			WillReturnResult(sqlmock.NewResult(0, 1))
		if err := r.UpdateResultInScope(ctx, "d1", "completed", 1, 2, 3, true, nil, "detail", Completeness{}, scope); err != nil {
			t.Fatalf("UpdateResultInScope: %v", err)
		}

		if err := mock.ExpectationsWereMet(); err != nil {
			t.Error(err)
		}
	})

	t.Run("health_runs", func(t *testing.T) {
		db, mock := newMock(t)
		r := NewHealthRepository(db)

		mock.ExpectQuery("FROM health_runs WHERE organization_id = ANY").
			WithArgs([]string{scopeOrgA}, "failed", 25, 50).WillReturnRows(healthRow("secret"))
		out, err := r.ListInScope(ctx, 25, 50, "failed", scope)
		if err != nil || len(out) != 1 {
			t.Fatalf("ListInScope: %v %+v", err, out)
		}
		if out[0].CallbackToken != "" {
			t.Error("ListInScope returned a callback token; the scoped list must strip it exactly as List does")
		}

		mock.ExpectQuery(`SELECT COUNT\(\*\) FROM health_runs WHERE organization_id = ANY`).
			WithArgs([]string{scopeOrgA}, "failed").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(2))
		if n, err := r.CountRunsInScope(ctx, "failed", scope); err != nil || n != 2 {
			t.Fatalf("CountRunsInScope = %d, %v", n, err)
		}

		mock.ExpectQuery("FROM health_runs WHERE organization_id = ANY").
			WithArgs([]string{scopeOrgA}, "h1").WillReturnRows(healthRow("secret"))
		if got, err := r.GetByIDInScope(ctx, "h1", scope); err != nil || got == nil {
			t.Fatalf("GetByIDInScope: %v %+v", err, got)
		}

		mock.ExpectQuery("FROM health_runs WHERE organization_id = ANY").
			WithArgs([]string{scopeOrgA}, "elsewhere").WillReturnError(sql.ErrNoRows)
		if got, err := r.GetByIDInScope(ctx, "elsewhere", scope); err != nil || got != nil {
			t.Errorf("a run outside the scope must be (nil, nil), got %+v %v", got, err)
		}

		mock.ExpectExec("UPDATE health_runs SET status.+organization_id = ANY").
			WithArgs("h1", "completed", true, true, true, nil, "detail", []string{scopeOrgA}).
			WillReturnResult(sqlmock.NewResult(0, 1))
		if err := r.UpdateResultInScope(ctx, "h1", "completed", true, true, true, nil, "detail", scope); err != nil {
			t.Fatalf("UpdateResultInScope: %v", err)
		}

		if err := mock.ExpectationsWereMet(); err != nil {
			t.Error(err)
		}
	})

	t.Run("drift_records", func(t *testing.T) {
		db, mock := newMock(t)
		r := NewDriftRecordRepository(db)

		// The organization is $1 and the FILTERS are numbered after it. This is
		// the case a hand-renumbered second copy of driftRecordFilter would get
		// wrong, and getting it wrong means a status filter evaluated against a
		// uuid array.
		mock.ExpectQuery("FROM drift_records WHERE organization_id = ANY.+AND status = ANY.+AND source_id").
			WithArgs([]string{scopeOrgA}, []string{"open"}, "s1", 100, 0).
			WillReturnRows(driftRecordRow("r1", "open"))
		if out, err := r.ListInScope(ctx, []string{"open"}, "s1", "", 0, 0, nil, nil, scope); err != nil || len(out) != 1 {
			t.Fatalf("ListInScope: %v %+v", err, out)
		}

		mock.ExpectQuery(`SELECT COUNT\(\*\) FROM drift_records WHERE organization_id = ANY`).
			WithArgs([]string{scopeOrgA}).WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(4))
		if n, err := r.CountRecordsInScope(ctx, nil, "", "", nil, nil, scope); err != nil || n != 4 {
			t.Fatalf("CountRecordsInScope = %d, %v", n, err)
		}

		mock.ExpectQuery("SELECT status, COUNT.+WHERE organization_id = ANY").
			WithArgs([]string{scopeOrgA}).
			WillReturnRows(sqlmock.NewRows([]string{"status", "count"}).AddRow("open", 4))
		if m, err := r.CountsByStatusInScope(ctx, scope); err != nil || m["open"] != 4 {
			t.Fatalf("CountsByStatusInScope = %v, %v", m, err)
		}

		mock.ExpectQuery("FROM drift_records WHERE organization_id = ANY.+AND id").
			WithArgs([]string{scopeOrgA}, "r1").WillReturnRows(driftRecordRow("r1", "open"))
		if got, err := r.GetByIDInScope(ctx, "r1", scope); err != nil || got == nil {
			t.Fatalf("GetByIDInScope: %v %+v", err, got)
		}

		mock.ExpectQuery("UPDATE drift_records.+acknowledged.+organization_id = ANY").
			WithArgs("r1", "alice", "note", []string{scopeOrgA}).WillReturnRows(driftRecordRow("r1", "open"))
		if got, err := r.AcknowledgeInScope(ctx, "r1", "alice", "note", scope); err != nil || got == nil {
			t.Fatalf("AcknowledgeInScope: %v %+v", err, got)
		}

		mock.ExpectQuery("UPDATE drift_records SET status='resolved'.+organization_id = ANY").
			WithArgs("r1", []string{scopeOrgA}).WillReturnRows(driftRecordRow("r1", "open"))
		if got, err := r.ResolveInScope(ctx, "r1", scope); err != nil || got == nil {
			t.Fatalf("ResolveInScope: %v %+v", err, got)
		}

		mock.ExpectExec("UPDATE drift_records SET status='resolved'.+organization_id = ANY").
			WithArgs("s1", "k", []string{scopeOrgA}).WillReturnResult(sqlmock.NewResult(0, 1))
		if ok, err := r.ResolveCleanInScope(ctx, "s1", "k", scope); err != nil || !ok {
			t.Fatalf("ResolveCleanInScope = %v, %v", ok, err)
		}

		// The scope lands on the SOURCE SELECT, as $17, so the detection is
		// produced only when the source the record inherits from is one this
		// authority may reach.
		mock.ExpectQuery("INSERT INTO drift_records.+s.organization_id = ANY").
			WithArgs("s1", "k", nil, nil, "run", "warning", 1, 0, 0, nil, nil,
				false, 0, 0, false, false, []string{scopeOrgA}).
			WillReturnRows(driftRecordRow("r1", "open"))
		if _, err := r.UpsertDetectionInScope(ctx, &Detection{SourceID: "s1", StateKey: "k", Origin: "run", Added: 1}, scope); err != nil {
			t.Fatalf("UpsertDetectionInScope: %v", err)
		}

		if err := mock.ExpectationsWereMet(); err != nil {
			t.Error(err)
		}
	})
}

// A platform admin reads and writes unfiltered — TSM's only principal that is
// not somebody's tenant admin, and the only one that can reach a row whose
// organization_id is still NULL (a database restored from a pre-000034 backup
// holds those, and the organization predicate can never match one).
//
// Asserted as "the statement carried no organization argument", which is what
// distinguishes the unscoped query from the scoped one.
func TestCallbackRoots_ScopedReads_PlatformAdminIsUnfiltered(t *testing.T) {
	admin := tenantscope.Scope{PlatformAdmin: true}

	db, mock := newMock(t)
	drift := NewDriftRepository(db)
	health := NewHealthRepository(db)
	records := NewDriftRecordRepository(db)

	mock.ExpectQuery("FROM drift_runs ORDER BY").WithArgs(50, 0).WillReturnRows(driftRow(""))
	if _, err := drift.ListInScope(ctx, 0, 0, "", admin); err != nil {
		t.Fatalf("drift ListInScope(platform admin): %v", err)
	}
	mock.ExpectQuery("FROM drift_runs WHERE id").WithArgs("d1").WillReturnRows(driftRow(""))
	if _, err := drift.GetByIDInScope(ctx, "d1", admin); err != nil {
		t.Fatalf("drift GetByIDInScope(platform admin): %v", err)
	}

	mock.ExpectQuery("FROM health_runs ORDER BY").WithArgs(50, 0).WillReturnRows(healthRow(""))
	if _, err := health.ListInScope(ctx, 0, 0, "", admin); err != nil {
		t.Fatalf("health ListInScope(platform admin): %v", err)
	}
	mock.ExpectQuery("FROM health_runs WHERE id").WithArgs("h1").WillReturnRows(healthRow(""))
	if _, err := health.GetByIDInScope(ctx, "h1", admin); err != nil {
		t.Fatalf("health GetByIDInScope(platform admin): %v", err)
	}

	mock.ExpectQuery("FROM drift_records WHERE 1=1").WithArgs(100, 0).WillReturnRows(driftRecordRow("r1", "open"))
	if _, err := records.ListInScope(ctx, nil, "", "", 0, 0, nil, nil, admin); err != nil {
		t.Fatalf("records ListInScope(platform admin): %v", err)
	}
	mock.ExpectQuery("FROM drift_records WHERE id").WithArgs("r1").WillReturnRows(driftRecordRow("r1", "open"))
	if _, err := records.GetByIDInScope(ctx, "r1", admin); err != nil {
		t.Fatalf("records GetByIDInScope(platform admin): %v", err)
	}
	mock.ExpectExec("UPDATE drift_records SET status='resolved'").WithArgs("s1", "k").
		WillReturnResult(sqlmock.NewResult(0, 1))
	if _, err := records.ResolveCleanInScope(ctx, "s1", "k", admin); err != nil {
		t.Fatalf("records ResolveCleanInScope(platform admin): %v", err)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// TestDriftRecordFilter_NumbersPlaceholdersAfterTheSeededArgs is the unit under
// the ListInScope case above, asserted directly because it is the piece a second
// copy would get wrong quietly. With no seed the first filter is $1; seeded with
// the organization array it must be $2, and every later one must shift with it.
func TestDriftRecordFilter_NumbersPlaceholdersAfterTheSeededArgs(t *testing.T) {
	unseeded, args := driftRecordFilter(nil, []string{"open"}, "s1", "critical", nil, nil)
	if unseeded != " AND status = ANY($1) AND source_id = $2 AND severity = $3" || len(args) != 3 {
		t.Fatalf("unseeded clause = %q with %d args", unseeded, len(args))
	}
	seeded, args := driftRecordFilter([]any{[]string{scopeOrgA}}, []string{"open"}, "s1", "critical", nil, nil)
	if seeded != " AND status = ANY($2) AND source_id = $3 AND severity = $4" || len(args) != 4 {
		t.Fatalf("seeded clause = %q with %d args; the organization is $1, so every filter "+
			"shifts by one. A clause that did not shift binds a status filter against a uuid array.",
			seeded, len(args))
	}
}

// TestReplaceForStateInScope_ChecksTheSourceBeforeRewriting covers the write
// side of the INHERITED table on this path.
//
// state_module_refs has no organization_id — it inherits through source_id — so
// the check is a separate ownership question asked before the transaction opens.
// The order matters: the statement DELETEs before it INSERTs, so an ownership
// test that ran inside the transaction would already have destroyed the rows it
// was about to decline to replace.
func TestReplaceForStateInScope_ChecksTheSourceBeforeRewriting(t *testing.T) {
	scope := tenantscope.Scope{OrgIDs: []string{scopeOrgA}}
	version := "5.3.0"
	refs := []StateModuleRef{{ModuleSource: "acme/vpc/aws", ModuleVersion: &version, RegistryHost: "registry.terraform.io"}}

	t.Run("an empty scope opens no transaction", func(t *testing.T) {
		db, mock := newMock(t)
		r := NewStateModuleRefRepository(db)
		if err := r.ReplaceForStateInScope(ctx, "s1", "k", refs, tenantscope.Scope{}); !errors.Is(err, ErrNotInScope) {
			t.Fatalf("= %v, want ErrNotInScope", err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Error(err)
		}
	})

	t.Run("a source outside the scope is refused before the DELETE", func(t *testing.T) {
		db, mock := newMock(t)
		r := NewStateModuleRefRepository(db)
		mock.ExpectQuery("SELECT EXISTS.+FROM state_sources WHERE id = .+organization_id = ANY").
			WithArgs("s1", []string{scopeOrgA}).
			WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
		// No ExpectBegin: a transaction opened here would already be a DELETE
		// away from destroying the other organization's provenance.
		if err := r.ReplaceForStateInScope(ctx, "s1", "k", refs, scope); !errors.Is(err, ErrNotInScope) {
			t.Fatalf("= %v, want ErrNotInScope", err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Error(err)
		}
	})

	t.Run("an owned source is rewritten", func(t *testing.T) {
		db, mock := newMock(t)
		r := NewStateModuleRefRepository(db)
		mock.ExpectQuery("SELECT EXISTS.+FROM state_sources").
			WithArgs("s1", []string{scopeOrgA}).
			WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
		mock.ExpectBegin()
		mock.ExpectExec("DELETE FROM state_module_refs").WithArgs("s1", "k").
			WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectExec("INSERT INTO state_module_refs").
			WithArgs("s1", "k", "acme/vpc/aws", &version, "registry.terraform.io").
			WillReturnResult(sqlmock.NewResult(1, 1))
		mock.ExpectCommit()
		if err := r.ReplaceForStateInScope(ctx, "s1", "k", refs, scope); err != nil {
			t.Fatalf("ReplaceForStateInScope: %v", err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Error(err)
		}
	})

	t.Run("a platform admin runs the unscoped statement", func(t *testing.T) {
		db, mock := newMock(t)
		r := NewStateModuleRefRepository(db)
		// No EXISTS probe: the skip branch means the ownership question was not
		// asked, which is what distinguishes it from the scoped path.
		mock.ExpectBegin()
		mock.ExpectExec("DELETE FROM state_module_refs").WithArgs("s1", "k").
			WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectCommit()
		if err := r.ReplaceForStateInScope(ctx, "s1", "k", nil, tenantscope.Scope{PlatformAdmin: true}); err != nil {
			t.Fatalf("ReplaceForStateInScope(platform admin): %v", err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Error(err)
		}
	})

	t.Run("a failed ownership probe is reported, not assumed", func(t *testing.T) {
		db, _ := newMock(t)
		r := NewStateModuleRefRepository(db)
		if err := r.ReplaceForStateInScope(ctx, "s1", "k", refs, scope); err == nil {
			t.Error("a probe that could not run must not be read as 'owned'")
		}
	})
}

// TestCallbackRoots_ScopedReaders_ReportQueryErrors keeps the error arms from
// being swallowed into an empty result, which would read as a refusal.
func TestCallbackRoots_ScopedReaders_ReportQueryErrors(t *testing.T) {
	scope := tenantscope.Scope{OrgIDs: []string{scopeOrgA}}

	db, mock := newMock(t)
	drift := NewDriftRepository(db)
	health := NewHealthRepository(db)
	records := NewDriftRecordRepository(db)

	mock.ExpectQuery("FROM drift_runs WHERE organization_id = ANY").WillReturnError(errDB)
	if _, err := drift.ListInScope(ctx, 0, 0, "", scope); err == nil {
		t.Error("drift ListInScope swallowed the query error")
	}
	mock.ExpectQuery("FROM drift_runs WHERE organization_id = ANY").WillReturnError(errDB)
	if _, err := drift.ListInScope(ctx, 0, 0, "failed", scope); err == nil {
		t.Error("drift ListInScope (status filter) swallowed the query error")
	}
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM drift_runs`).WillReturnError(errDB)
	if _, err := drift.CountRunsInScope(ctx, "failed", scope); err == nil {
		t.Error("drift CountRunsInScope swallowed the query error")
	}
	mock.ExpectQuery("FROM drift_runs WHERE organization_id = ANY").WillReturnError(errDB)
	if _, err := drift.GetByIDInScope(ctx, "d1", scope); err == nil {
		t.Error("drift GetByIDInScope swallowed the query error")
	}
	mock.ExpectExec("UPDATE drift_runs SET status").WillReturnError(errDB)
	if err := drift.UpdateResultInScope(ctx, "d1", "completed", 0, 0, 0, false, []byte(`[]`), "", Completeness{}, scope); err == nil {
		t.Error("drift UpdateResultInScope swallowed the exec error")
	}

	mock.ExpectQuery("FROM health_runs WHERE organization_id = ANY").WillReturnError(errDB)
	if _, err := health.ListInScope(ctx, 0, 0, "", scope); err == nil {
		t.Error("health ListInScope swallowed the query error")
	}
	mock.ExpectQuery("FROM health_runs WHERE organization_id = ANY").WillReturnError(errDB)
	if _, err := health.ListInScope(ctx, 0, 0, "failed", scope); err == nil {
		t.Error("health ListInScope (status filter) swallowed the query error")
	}
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM health_runs`).WillReturnError(errDB)
	if _, err := health.CountRunsInScope(ctx, "", scope); err == nil {
		t.Error("health CountRunsInScope swallowed the query error")
	}
	mock.ExpectQuery("FROM health_runs WHERE organization_id = ANY").WillReturnError(errDB)
	if _, err := health.GetByIDInScope(ctx, "h1", scope); err == nil {
		t.Error("health GetByIDInScope swallowed the query error")
	}
	mock.ExpectExec("UPDATE health_runs SET status").WillReturnError(errDB)
	if err := health.UpdateResultInScope(ctx, "h1", "completed", true, true, true, []byte(`{}`), "", scope); err == nil {
		t.Error("health UpdateResultInScope swallowed the exec error")
	}

	mock.ExpectQuery("FROM drift_records WHERE organization_id = ANY").WillReturnError(errDB)
	if _, err := records.ListInScope(ctx, nil, "", "", 0, 0, nil, nil, scope); err == nil {
		t.Error("records ListInScope swallowed the query error")
	}
	mock.ExpectQuery("SELECT status, COUNT").WillReturnError(errDB)
	if _, err := records.CountsByStatusInScope(ctx, scope); err == nil {
		t.Error("records CountsByStatusInScope swallowed the query error")
	}
	mock.ExpectQuery("FROM drift_records WHERE organization_id = ANY").WillReturnError(errDB)
	if _, err := records.GetByIDInScope(ctx, "r1", scope); err == nil {
		t.Error("records GetByIDInScope swallowed the query error")
	}
	mock.ExpectQuery("UPDATE drift_records").WillReturnError(errDB)
	if _, err := records.AcknowledgeInScope(ctx, "r1", "alice", "", scope); err == nil {
		t.Error("records AcknowledgeInScope swallowed the query error")
	}
	mock.ExpectQuery("UPDATE drift_records").WillReturnError(errDB)
	if _, err := records.ResolveInScope(ctx, "r1", scope); err == nil {
		t.Error("records ResolveInScope swallowed the query error")
	}

	// A row outside the scope is (nil, nil) on both mutating readers — the same
	// shape a missing row has, so a caller cannot tell them apart.
	mock.ExpectQuery("UPDATE drift_records").WillReturnError(sql.ErrNoRows)
	if got, err := records.AcknowledgeInScope(ctx, "r1", "alice", "", scope); got != nil || err != nil {
		t.Errorf("AcknowledgeInScope on an unreachable row = %+v, %v; want (nil, nil)", got, err)
	}
	mock.ExpectQuery("UPDATE drift_records").WillReturnError(sql.ErrNoRows)
	if got, err := records.ResolveInScope(ctx, "r1", scope); got != nil || err != nil {
		t.Errorf("ResolveInScope on an unreachable row = %+v, %v; want (nil, nil)", got, err)
	}
}
