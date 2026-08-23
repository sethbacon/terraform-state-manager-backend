package repositories

import (
	"reflect"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
)

// ---------------------------------------------------------------------------
// StateAnalysisRepository
// ---------------------------------------------------------------------------

func TestStateAnalysisUpsert(t *testing.T) {
	db, mock := newMock(t)
	r := NewStateAnalysisRepository(db)

	mock.ExpectExec("INSERT INTO state_analyses").
		WithArgs("s1", "app.tfstate", "2048|2026-06-11T00:00:00Z", int64(2048), "1.9.5", int64(7), "lin-1",
			2, 2, 0, 2, []byte(`{"aws":2}`), []byte(`{"aws_instance":1,"aws_vpc":1}`)).
		WillReturnResult(sqlmock.NewResult(0, 1))

	err := r.Upsert(ctx, &StateAnalysis{
		SourceID: "s1", StateKey: "app.tfstate", VersionMarker: "2048|2026-06-11T00:00:00Z",
		Size: 2048, TerraformVersion: "1.9.5", Serial: 7, Lineage: "lin-1",
		RUM: 2, ManagedResources: 2, DataSources: 0, TotalResources: 2,
		Providers:     map[string]int{"aws": 2},
		ResourceTypes: map[string]int{"aws_instance": 1, "aws_vpc": 1},
	})
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	// Nil maps marshal to {} rather than null.
	mock.ExpectExec("INSERT INTO state_analyses").
		WithArgs("s1", "empty.tfstate", "", int64(0), "", int64(0), "",
			0, 0, 0, 0, []byte(`{}`), []byte(`{}`)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := r.Upsert(ctx, &StateAnalysis{SourceID: "s1", StateKey: "empty.tfstate"}); err != nil {
		t.Fatalf("Upsert empty: %v", err)
	}

	mock.ExpectExec("INSERT INTO state_analyses").WillReturnError(errDB)
	if err := r.Upsert(ctx, &StateAnalysis{SourceID: "s1", StateKey: "x"}); err == nil {
		t.Error("Upsert: expected error")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestStateAnalysisMarkers(t *testing.T) {
	db, mock := newMock(t)
	r := NewStateAnalysisRepository(db)

	mock.ExpectQuery("SELECT state_key, version_marker FROM state_analyses").WithArgs("s1").
		WillReturnRows(sqlmock.NewRows([]string{"state_key", "version_marker"}).
			AddRow("app.tfstate", "2048|t1").AddRow("net.tfstate", "1024|t2"))
	markers, err := r.Markers(ctx, "s1")
	if err != nil {
		t.Fatalf("Markers: %v", err)
	}
	if len(markers) != 2 || markers["app.tfstate"] != "2048|t1" {
		t.Errorf("markers = %v", markers)
	}

	mock.ExpectQuery("SELECT state_key, version_marker FROM state_analyses").WillReturnError(errDB)
	if _, err := r.Markers(ctx, "s1"); err == nil {
		t.Error("Markers: expected error")
	}
}

func TestStateAnalysisSizes(t *testing.T) {
	db, mock := newMock(t)
	r := NewStateAnalysisRepository(db)

	mock.ExpectQuery("SELECT state_key, size FROM state_analyses").WithArgs("s1").
		WillReturnRows(sqlmock.NewRows([]string{"state_key", "size"}).
			AddRow("ws-abc", 28412).AddRow("ws-def", 911))
	sizes, err := r.Sizes(ctx, "s1")
	if err != nil {
		t.Fatalf("Sizes: %v", err)
	}
	if len(sizes) != 2 || sizes["ws-abc"] != 28412 {
		t.Errorf("sizes = %v", sizes)
	}

	mock.ExpectQuery("SELECT state_key, size FROM state_analyses").WillReturnError(errDB)
	if _, err := r.Sizes(ctx, "s1"); err == nil {
		t.Error("Sizes: expected error")
	}
}

func TestStateAnalysisDeletes(t *testing.T) {
	db, mock := newMock(t)
	r := NewStateAnalysisRepository(db)

	mock.ExpectExec("DELETE FROM state_analyses WHERE source_id = .+ AND NOT").
		WithArgs("s1", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 3))
	if err := r.DeleteMissing(ctx, "s1", []string{"app.tfstate"}); err != nil {
		t.Fatalf("DeleteMissing: %v", err)
	}

	mock.ExpectExec("DELETE FROM state_analyses WHERE source_id = .+ AND state_key").
		WithArgs("s1", "gone.tfstate").
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := r.Delete(ctx, "s1", "gone.tfstate"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestStateAnalysisAggregates(t *testing.T) {
	db, mock := newMock(t)
	r := NewStateAnalysisRepository(db)

	mock.ExpectQuery(`SELECT COUNT\(\*\),`).WillReturnRows(
		sqlmock.NewRows([]string{"count", "rum", "managed", "data", "total"}).
			AddRow(168, 5200, 5210, 1000, 6210))
	totals, err := r.Totals(ctx)
	if err != nil {
		t.Fatalf("Totals: %v", err)
	}
	if totals.States != 168 || totals.RUM != 5200 || totals.TotalResources != 6210 {
		t.Errorf("totals = %+v", totals)
	}

	mock.ExpectQuery(`jsonb_each_text\(providers\)`).WillReturnRows(
		sqlmock.NewRows([]string{"key", "sum"}).AddRow("hashicorp/aws", 4000).AddRow("hashicorp/tfe", 1200))
	providers, err := r.ProviderCounts(ctx)
	if err != nil {
		t.Fatalf("ProviderCounts: %v", err)
	}
	if providers["hashicorp/aws"] != 4000 {
		t.Errorf("providers = %v", providers)
	}

	mock.ExpectQuery(`jsonb_each_text\(resource_types\)`).WillReturnRows(
		sqlmock.NewRows([]string{"key", "sum"}).AddRow("tfe_variable", 980))
	resTypes, err := r.ResourceTypeCounts(ctx)
	if err != nil {
		t.Fatalf("ResourceTypeCounts: %v", err)
	}
	if resTypes["tfe_variable"] != 980 {
		t.Errorf("resource types = %v", resTypes)
	}

	mock.ExpectQuery("SELECT CASE WHEN terraform_version").WillReturnRows(
		sqlmock.NewRows([]string{"v", "count"}).AddRow("1.9.5", 100).AddRow("unknown", 2))
	versions, err := r.VersionCounts(ctx)
	if err != nil {
		t.Fatalf("VersionCounts: %v", err)
	}
	if versions["unknown"] != 2 {
		t.Errorf("versions = %v", versions)
	}

	mock.ExpectQuery(`SELECT COUNT\(\*\),`).WillReturnError(errDB)
	if _, err := r.Totals(ctx); err == nil {
		t.Error("Totals: expected error")
	}
	mock.ExpectQuery(`jsonb_each_text\(providers\)`).WillReturnError(errDB)
	if _, err := r.ProviderCounts(ctx); err == nil {
		t.Error("ProviderCounts: expected error")
	}
}

func TestStateAnalysisStateVersions(t *testing.T) {
	db, mock := newMock(t)
	r := NewStateAnalysisRepository(db)

	mock.ExpectQuery("FROM state_analyses a").WillReturnRows(
		sqlmock.NewRows([]string{"source_id", "name", "state_key", "terraform_version", "rum"}).
			AddRow("s1", "prod", "a.tfstate", "1.5.7", 12).
			AddRow("s2", "dev", "b.tfstate", "", 0))
	got, err := r.StateVersions(ctx)
	if err != nil {
		t.Fatalf("StateVersions: %v", err)
	}
	if len(got) != 2 || got[0].SourceName != "prod" || got[0].RUM != 12 || got[1].TerraformVersion != "" {
		t.Errorf("StateVersions = %+v", got)
	}

	mock.ExpectQuery("FROM state_analyses a").WillReturnError(errDB)
	if _, err := r.StateVersions(ctx); err == nil {
		t.Error("StateVersions: expected error")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestStateAnalysisAllStates(t *testing.T) {
	db, mock := newMock(t)
	r := NewStateAnalysisRepository(db)

	cols := []string{
		"source_id", "name", "type", "state_key", "terraform_version", "serial", "lineage", "size",
		"rum", "managed_resources", "data_sources", "total_resources", "providers", "resource_types", "analyzed_at",
	}
	mock.ExpectQuery("FROM state_analyses a").WillReturnRows(
		sqlmock.NewRows(cols).
			AddRow("s1", "prod", "s3", "app.tfstate", "1.5.7", 10, "lin-1", 2048, 40, 38, 2, 42,
				[]byte(`{"aws":40}`), []byte(`{"aws_instance":12}`), "2026-06-18T00:00:00Z").
			AddRow("s2", "dev", "local", "x.tfstate", "", 1, "", 64, 0, 0, 0, 0,
				[]byte(`{}`), []byte(`{}`), "2026-06-18T00:00:00Z"))
	got, err := r.AllStates(ctx)
	if err != nil {
		t.Fatalf("AllStates: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("AllStates len = %d", len(got))
	}
	if got[0].SourceType != "s3" || got[0].Size != 2048 ||
		got[0].Providers["aws"] != 40 || got[0].ResourceTypes["aws_instance"] != 12 {
		t.Errorf("AllStates[0] = %+v", got[0])
	}
	if got[1].TerraformVersion != "" || len(got[1].Providers) != 0 {
		t.Errorf("AllStates[1] = %+v", got[1])
	}

	mock.ExpectQuery("FROM state_analyses a").WillReturnError(errDB)
	if _, err := r.AllStates(ctx); err == nil {
		t.Error("AllStates: expected error")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestSourceSyncStatus(t *testing.T) {
	db, mock := newMock(t)
	r := NewStateAnalysisRepository(db)

	mock.ExpectExec("INSERT INTO source_sync_status").
		WithArgs("s1", 165, 3, "read ws-1: 429").
		WillReturnResult(sqlmock.NewResult(0, 1))
	err := r.UpsertSyncStatus(ctx, &SourceSyncStatus{
		SourceID: "s1", StatesListed: 165, ReadErrors: 3, LastError: "read ws-1: 429",
	})
	if err != nil {
		t.Fatalf("UpsertSyncStatus: %v", err)
	}

	mock.ExpectQuery("FROM source_sync_status").WillReturnRows(
		sqlmock.NewRows([]string{"source_id", "last_sync_at", "states_listed", "read_errors", "last_error", "stored"}).
			AddRow("s1", "2026-06-11T09:00:00Z", 165, 3, "read ws-1: 429", 162).
			AddRow("s2", "2026-06-11T09:01:00Z", 3, 0, "", 3))
	statuses, err := r.SyncStatuses(ctx)
	if err != nil {
		t.Fatalf("SyncStatuses: %v", err)
	}
	if len(statuses) != 2 || statuses["s1"].StatesStored != 162 || statuses["s2"].ReadErrors != 0 {
		t.Errorf("statuses = %+v", statuses)
	}

	mock.ExpectQuery("FROM source_sync_status").WillReturnError(errDB)
	if _, err := r.SyncStatuses(ctx); err == nil {
		t.Error("SyncStatuses: expected error")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestStateAnalysisHistory(t *testing.T) {
	db, mock := newMock(t)
	r := NewStateAnalysisRepository(db)
	row := &StateAnalysis{SourceID: "s1", StateKey: "app.tfstate", VersionMarker: "10|x",
		Size: 10, TerraformVersion: "1.9.5", Serial: 7, RUM: 4, TotalResources: 5,
		Providers: map[string]int{"aws": 4}, ResourceTypes: map[string]int{"aws_instance": 4}}

	// Changed vs latest snapshot -> appended (the WHERE NOT EXISTS guard is
	// SQL-side; the contract here is rows-affected -> appended bool).
	mock.ExpectExec("INSERT INTO state_analysis_history").WillReturnResult(sqlmock.NewResult(0, 1))
	appended, err := r.AppendHistoryIfChanged(ctx, row)
	if err != nil || !appended {
		t.Fatalf("AppendHistoryIfChanged: %v appended=%v", err, appended)
	}

	// Identical to the latest snapshot -> guard suppresses the insert.
	mock.ExpectExec("INSERT INTO state_analysis_history").WillReturnResult(sqlmock.NewResult(0, 0))
	appended, err = r.AppendHistoryIfChanged(ctx, row)
	if err != nil || appended {
		t.Errorf("unchanged snapshot must not append: %v appended=%v", err, appended)
	}

	mock.ExpectExec("INSERT INTO state_analysis_history").WillReturnError(errDB)
	if _, err := r.AppendHistoryIfChanged(ctx, row); err == nil {
		t.Error("db error must surface")
	}

	// History returns snapshots newest-first with JSONB maps decoded.
	histCols := []string{"source_id", "state_key", "version_marker", "size", "terraform_version",
		"serial", "lineage", "rum", "managed_resources", "data_sources", "total_resources",
		"providers", "resource_types", "analyzed_at"}
	mock.ExpectQuery("FROM state_analysis_history").WithArgs("s1", "app.tfstate", 200).
		WillReturnRows(sqlmock.NewRows(histCols).
			AddRow("s1", "app.tfstate", "12|y", 12, "1.9.5", 8, "lin", 5, 5, 1, 6, []byte(`{"aws":5}`), []byte(`{"aws_instance":5}`), "2026-06-11T10:00:00Z").
			AddRow("s1", "app.tfstate", "10|x", 10, "1.9.5", 7, "lin", 4, 4, 1, 5, []byte(`{"aws":4}`), []byte(`{"aws_instance":4}`), "2026-06-10T10:00:00Z"))
	hist, err := r.History(ctx, "s1", "app.tfstate", 0)
	if err != nil || len(hist) != 2 {
		t.Fatalf("History: %v %+v", err, hist)
	}
	if hist[0].Serial != 8 || hist[0].Providers["aws"] != 5 || hist[1].RUM != 4 {
		t.Errorf("history rows = %+v", hist)
	}

	mock.ExpectExec("DELETE FROM state_analysis_history WHERE analyzed_at").
		WillReturnResult(sqlmock.NewResult(0, 9))
	if err := r.PruneHistory(ctx); err != nil {
		t.Errorf("PruneHistory: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// ptrInt/ptrInt64 build the *int/*int64 bounds StateQueryFilter uses.
func ptrInt(v int) *int       { return &v }
func ptrInt64(v int64) *int64 { return &v }

func TestBuildStateWhere(t *testing.T) {
	t.Run("empty filter yields no clause and no args", func(t *testing.T) {
		where, args := buildStateWhere(StateQueryFilter{}, nil)
		if where != "" || len(args) != 0 {
			t.Errorf("empty: where=%q args=%v", where, args)
		}
	})

	t.Run("clean predicates render in order with $N placeholders", func(t *testing.T) {
		ver := "1.5.7"
		where, args := buildStateWhere(StateQueryFilter{
			SourceIDs:    []string{"s1", "s2"},
			KeyContains:  "prod",
			VersionExact: &ver,
			RUMMin:       ptrInt(10),
			RUMMax:       ptrInt(100),
			SizeMax:      ptrInt64(2048),
		}, nil)
		want := "WHERE a.source_id IN ($1,$2) AND strpos(lower(a.state_key), $3) > 0 " +
			"AND a.terraform_version = $4 AND a.rum >= $5 AND a.rum <= $6 AND a.size <= $7"
		if where != want {
			t.Errorf("where =\n%q\nwant\n%q", where, want)
		}
		wantArgs := []any{"s1", "s2", "prod", "1.5.7", 10, 100, int64(2048)}
		if !reflect.DeepEqual(args, wantArgs) {
			t.Errorf("args = %v, want %v", args, wantArgs)
		}
	})

	t.Run("empty exact version matches the unknown/empty version", func(t *testing.T) {
		empty := ""
		where, args := buildStateWhere(StateQueryFilter{VersionExact: &empty}, nil)
		if where != "WHERE a.terraform_version = $1" || len(args) != 1 || args[0] != "" {
			t.Errorf("unknown-version: where=%q args=%v", where, args)
		}
	})
}

// analysisFullCols mirrors the FilterStates projection.
var analysisFullCols = []string{
	"source_id", "name", "type", "state_key", "terraform_version", "serial", "lineage", "size",
	"rum", "managed_resources", "data_sources", "total_resources", "providers", "resource_types", "analyzed_at",
}

func TestFilterStates(t *testing.T) {
	db, mock := newMock(t)
	r := NewStateAnalysisRepository(db)

	ver := "1.5.7"
	// The clean predicates must be threaded through as positional args.
	mock.ExpectQuery("FROM state_analyses a").
		WithArgs("s1", "prod", "1.5.7", 10).
		WillReturnRows(sqlmock.NewRows(analysisFullCols).
			AddRow("s1", "prod", "s3", "envs/prod/app.tfstate", "1.5.7", int64(10), "lin-1", int64(2048),
				40, 38, 2, 42, []byte(`{"aws":40}`), []byte(`{"aws_instance":12}`), "2026-06-18T00:00:00Z"))

	rows, err := r.FilterStates(ctx, StateQueryFilter{
		SourceIDs: []string{"s1"}, KeyContains: "prod", VersionExact: &ver, RUMMin: ptrInt(10),
	})
	if err != nil {
		t.Fatalf("FilterStates: %v", err)
	}
	if len(rows) != 1 || rows[0].StateKey != "envs/prod/app.tfstate" || rows[0].Providers["aws"] != 40 {
		t.Errorf("unexpected rows: %+v", rows)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestPreviewStatesWithTotals(t *testing.T) {
	db, mock := newMock(t)
	r := NewStateAnalysisRepository(db)

	cols := []string{
		"source_id", "name", "type", "state_key", "terraform_version", "serial", "size",
		"rum", "managed_resources", "data_sources", "total_resources", "analyzed_at",
		"full_count", "sum_rum", "sum_managed", "sum_data", "sum_total",
	}
	// No filter: the only positional arg is the LIMIT. Window aggregates repeat.
	mock.ExpectQuery(`COUNT\(\*\) OVER`).
		WithArgs(500).
		WillReturnRows(sqlmock.NewRows(cols).
			AddRow("s1", "prod", "s3", "envs/prod/app.tfstate", "1.5.7", int64(10), int64(2048),
				40, 38, 2, 42, "2026-06-18T00:00:00Z", 2, 48, 46, 2, 50).
			AddRow("s2", "dev", "local", "dev/net.tfstate", "0.14.11", int64(5), int64(512),
				8, 8, 0, 8, "2026-06-18T00:00:00Z", 2, 48, 46, 2, 50))

	rows, agg, err := r.PreviewStatesWithTotals(ctx, StateQueryFilter{}, 500)
	if err != nil {
		t.Fatalf("PreviewStatesWithTotals: %v", err)
	}
	if len(rows) != 2 || rows[0].StateKey != "envs/prod/app.tfstate" {
		t.Errorf("unexpected rows: %+v", rows)
	}
	if agg.Matched != 2 || agg.RUM != 48 || agg.ManagedResources != 46 || agg.DataSources != 2 || agg.TotalResources != 50 {
		t.Errorf("unexpected aggregate: %+v", agg)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestStatesByVersionExact(t *testing.T) {
	db, mock := newMock(t)
	r := NewStateAnalysisRepository(db)

	// The version predicate binds via $1 and the cap via $2; full_count (window)
	// repeats on every row and is read into total.
	mock.ExpectQuery(`WHERE a.terraform_version = \$1`).
		WithArgs("1.5.7", 500).
		WillReturnRows(sqlmock.NewRows([]string{"source_id", "name", "state_key", "terraform_version", "rum", "full_count"}).
			AddRow("s2", "dev", "d.tfstate", "1.5.7", 12, 3).
			AddRow("s2", "dev", "e.tfstate", "1.5.7", 4, 3))

	states, total, err := r.StatesByVersionExact(ctx, "1.5.7", 500)
	if err != nil {
		t.Fatalf("StatesByVersionExact: %v", err)
	}
	if len(states) != 2 || states[0].StateKey != "d.tfstate" || states[0].RUM != 12 {
		t.Errorf("states = %+v", states)
	}
	if total != 3 { // full match exceeds the two returned rows
		t.Errorf("total = %d, want 3", total)
	}

	mock.ExpectQuery(`WHERE a.terraform_version = \$1`).WillReturnError(errDB)
	if _, _, err := r.StatesByVersionExact(ctx, "", 500); err == nil {
		t.Error("expected error")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}
