package repositories

import (
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
