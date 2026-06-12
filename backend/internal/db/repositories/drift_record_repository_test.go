package repositories

import (
	"errors"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/lib/pq"
)

var driftRecordCols = []string{"id", "source_id", "state_key", "pipeline_connection_id", "last_run_id",
	"origin", "severity", "added", "changed", "destroyed", "summary", "status", "acknowledged_by",
	"acknowledged_at", "ack_note", "resolved_at", "external_ref", "detections", "first_detected_at",
	"last_detected_at"}

func driftRecordRow(id, status string) *sqlmock.Rows {
	return sqlmock.NewRows(driftRecordCols).
		AddRow(id, "s1", "app.tfstate", nil, nil, "run", "warning", 1, 2, 0, []byte(`[]`), status,
			"", nil, "", nil, nil, 1, "2026-06-11", "2026-06-11")
}

func TestDriftSeverity(t *testing.T) {
	if DriftSeverity(0) != "warning" || DriftSeverity(3) != "critical" {
		t.Error("destroyed resources must classify critical, otherwise warning")
	}
}

func TestDriftRecordRepository_UpsertDetection(t *testing.T) {
	db, mock := newMock(t)
	r := NewDriftRecordRepository(db)

	mock.ExpectQuery("INSERT INTO drift_records").
		WillReturnRows(driftRecordRow("r1", "open"))
	rec, err := r.UpsertDetection(ctx, &Detection{SourceID: "s1", StateKey: "app.tfstate", Origin: "run", Added: 1, Changed: 2})
	if err != nil || rec.ID != "r1" {
		t.Fatalf("UpsertDetection: %v %+v", err, rec)
	}

	// A resolved record already holding the external_ref (pipeline retry after
	// auto-resolve) trips the partial unique index; the replay must be returned
	// as-is, not surfaced as an error.
	ref := "run-42"
	mock.ExpectQuery("INSERT INTO drift_records").
		WillReturnError(&pq.Error{Code: "23505"})
	mock.ExpectQuery("FROM drift_records WHERE source_id .+ external_ref").WithArgs("s1", ref).
		WillReturnRows(driftRecordRow("r-old", "resolved"))
	rec, err = r.UpsertDetection(ctx, &Detection{SourceID: "s1", StateKey: "app.tfstate", Origin: "ingest", ExternalRef: &ref})
	if err != nil || rec.ID != "r-old" {
		t.Fatalf("external_ref replay: %v %+v", err, rec)
	}

	// Other DB errors pass through.
	mock.ExpectQuery("INSERT INTO drift_records").WillReturnError(errDB)
	if _, err := r.UpsertDetection(ctx, &Detection{SourceID: "s1", StateKey: "k", Origin: "run"}); err == nil {
		t.Error("db error must surface")
	}
}

func TestDriftRecordRepository_ResolveClean(t *testing.T) {
	db, mock := newMock(t)
	r := NewDriftRecordRepository(db)

	mock.ExpectExec("UPDATE drift_records SET status='resolved'").WithArgs("s1", "app.tfstate").
		WillReturnResult(sqlmock.NewResult(0, 1))
	resolved, err := r.ResolveClean(ctx, "s1", "app.tfstate")
	if err != nil || !resolved {
		t.Errorf("ResolveClean: %v resolved=%v", err, resolved)
	}

	mock.ExpectExec("UPDATE drift_records SET status='resolved'").WithArgs("s1", "other").
		WillReturnResult(sqlmock.NewResult(0, 0))
	resolved, err = r.ResolveClean(ctx, "s1", "other")
	if err != nil || resolved {
		t.Errorf("clean state with no record: %v resolved=%v", err, resolved)
	}
}

func TestDriftRecordRepository_ListAndCounts(t *testing.T) {
	db, mock := newMock(t)
	r := NewDriftRecordRepository(db)

	mock.ExpectQuery("FROM drift_records WHERE 1=1 AND status = ANY").
		WithArgs(pq.Array([]string{"open", "acknowledged"}), "s1", 100).
		WillReturnRows(driftRecordRow("r1", "open"))
	recs, err := r.List(ctx, []string{"open", "acknowledged"}, "s1", "", 0)
	if err != nil || len(recs) != 1 || recs[0].Status != "open" {
		t.Fatalf("List: %v %+v", err, recs)
	}

	// No filters: only the limit binds.
	mock.ExpectQuery("FROM drift_records WHERE 1=1 ORDER BY").WithArgs(25).
		WillReturnRows(sqlmock.NewRows(driftRecordCols))
	if recs, err := r.List(ctx, nil, "", "", 25); err != nil || len(recs) != 0 {
		t.Fatalf("unfiltered List: %v %+v", err, recs)
	}

	mock.ExpectQuery("SELECT status, COUNT").
		WillReturnRows(sqlmock.NewRows([]string{"status", "count"}).AddRow("open", 3).AddRow("resolved", 7))
	counts, err := r.CountsByStatus(ctx)
	if err != nil || counts["open"] != 3 || counts["resolved"] != 7 {
		t.Errorf("CountsByStatus: %v %+v", err, counts)
	}
}

func TestDriftRecordRepository_AcknowledgeAndResolve(t *testing.T) {
	db, mock := newMock(t)
	r := NewDriftRecordRepository(db)

	mock.ExpectQuery("UPDATE drift_records").WithArgs("r1", "alice", "known cert rotation").
		WillReturnRows(driftRecordRow("r1", "acknowledged"))
	rec, err := r.Acknowledge(ctx, "r1", "alice", "known cert rotation")
	if err != nil || rec == nil || rec.Status != "acknowledged" {
		t.Fatalf("Acknowledge: %v %+v", err, rec)
	}

	// Not-open (or missing) records return (nil, nil) — handlers disambiguate.
	mock.ExpectQuery("UPDATE drift_records").WithArgs("r2", "alice", "").
		WillReturnRows(sqlmock.NewRows(driftRecordCols))
	if rec, err := r.Acknowledge(ctx, "r2", "alice", ""); err != nil || rec != nil {
		t.Errorf("ack of non-open record: %v %+v", err, rec)
	}

	mock.ExpectQuery("UPDATE drift_records SET status='resolved'").WithArgs("r1").
		WillReturnRows(driftRecordRow("r1", "resolved"))
	rec, err = r.Resolve(ctx, "r1")
	if err != nil || rec == nil || rec.Status != "resolved" {
		t.Fatalf("Resolve: %v %+v", err, rec)
	}
}

func TestDriftRecordRepository_GetByID(t *testing.T) {
	db, mock := newMock(t)
	r := NewDriftRecordRepository(db)

	mock.ExpectQuery("FROM drift_records WHERE id").WithArgs("r1").
		WillReturnRows(driftRecordRow("r1", "open"))
	rec, err := r.GetByID(ctx, "r1")
	if err != nil || rec == nil || rec.StateKey != "app.tfstate" {
		t.Fatalf("GetByID: %v %+v", err, rec)
	}

	mock.ExpectQuery("FROM drift_records WHERE id").WithArgs("ghost").
		WillReturnRows(sqlmock.NewRows(driftRecordCols))
	if rec, err := r.GetByID(ctx, "ghost"); err != nil || rec != nil {
		t.Errorf("missing record: %v %+v", err, rec)
	}

	mock.ExpectQuery("FROM drift_records WHERE id").WillReturnError(errDB)
	if _, err := r.GetByID(ctx, "r1"); !errors.Is(err, errDB) {
		t.Errorf("db error must surface: %v", err)
	}
}
