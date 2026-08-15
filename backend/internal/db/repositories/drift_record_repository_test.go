package repositories

import (
	"errors"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/lib/pq"
)

var driftRecordCols = []string{"id", "source_id", "state_key", "pipeline_connection_id", "last_run_id",
	"origin", "severity", "added", "changed", "destroyed", "summary", "status", "acknowledged_by",
	"acknowledged_at", "ack_note", "resolved_at", "external_ref", "detections", "first_detected_at",
	"last_detected_at", "truncated", "omitted_entries", "omitted_attrs", "unparseable", "unmasked"}

func driftRecordRow(id, status string) *sqlmock.Rows {
	return sqlmock.NewRows(driftRecordCols).
		AddRow(id, "s1", "app.tfstate", nil, nil, "run", "warning", 1, 2, 0, []byte(`[]`), status,
			"", nil, "", nil, nil, 1, "2026-06-11", "2026-06-11",
			false, 0, 0, false, false)
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

// TestDetection_MarkTruncation pins the one-way widening: omission counts imply
// the flag, and the flag survives without counts. Narrowing is what would let a
// bounded summary be read as a complete one.
func TestDetection_MarkTruncation(t *testing.T) {
	cases := []struct {
		name                         string
		truncated                    bool
		omittedEntries, omittedAttrs int
		want                         bool
	}{
		{"complete stays complete", false, 0, 0, false},
		{"omitted entries imply truncated", false, 3, 0, true},
		{"omitted attrs imply truncated", false, 0, 7, true},
		{"flag without counts is believed", true, 0, 0, true},
		{"already agreeing is unchanged", true, 2, 2, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := &Detection{Completeness: Completeness{
				Truncated: c.truncated, OmittedEntries: c.omittedEntries, OmittedAttrs: c.omittedAttrs}}
			d.MarkTruncation()
			if d.Truncated != c.want {
				t.Errorf("Truncated = %v, want %v", d.Truncated, c.want)
			}
		})
	}
}

// TestDriftRecordRepository_PersistsCompletenessMarkers is the storage half of
// the round trip: the markers must be bound into the INSERT and read back off
// the returned row, not merely accepted by the handler above.
func TestDriftRecordRepository_PersistsCompletenessMarkers(t *testing.T) {
	db, mock := newMock(t)
	r := NewDriftRecordRepository(db)

	mock.ExpectQuery("INSERT INTO drift_records").
		WithArgs("s1", "app.tfstate", nil, nil, "ingest", "warning", 1, 0, 0, nil, nil,
			true, 5, 9, true, true).
		WillReturnRows(sqlmock.NewRows(driftRecordCols).
			AddRow("r1", "s1", "app.tfstate", nil, nil, "ingest", "warning", 1, 0, 0,
				[]byte(`[]`), "open", "", nil, "", nil, nil, 1, "2026-06-11", "2026-06-11",
				true, 5, 9, true, true))

	rec, err := r.UpsertDetection(ctx, &Detection{
		SourceID: "s1", StateKey: "app.tfstate", Origin: "ingest", Added: 1,
		Completeness: Completeness{
			Truncated: true, OmittedEntries: 5, OmittedAttrs: 9, Unparseable: true, Unmasked: true},
	})
	if err != nil {
		t.Fatalf("UpsertDetection: %v", err)
	}
	if !rec.Truncated || rec.OmittedEntries != 5 || rec.OmittedAttrs != 9 || !rec.Unparseable || !rec.Unmasked {
		t.Errorf("markers lost in the round trip: %+v", rec)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("markers must be bound into the INSERT: %v", err)
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
		WithArgs(pq.Array([]string{"open", "acknowledged"}), "s1", 100, 0).
		WillReturnRows(driftRecordRow("r1", "open"))
	recs, err := r.List(ctx, []string{"open", "acknowledged"}, "s1", "", 0, 0, nil, nil)
	if err != nil || len(recs) != 1 || recs[0].Status != "open" {
		t.Fatalf("List: %v %+v", err, recs)
	}

	// No filters: only the window binds.
	mock.ExpectQuery("FROM drift_records WHERE 1=1 ORDER BY").WithArgs(25, 50).
		WillReturnRows(sqlmock.NewRows(driftRecordCols))
	if recs, err := r.List(ctx, nil, "", "", 25, 50, nil, nil); err != nil || len(recs) != 0 {
		t.Fatalf("unfiltered List: %v %+v", err, recs)
	}

	// A last-detected date range binds both bounds; the filtered count reuses
	// the same WHERE tail.
	start := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 7, 9, 0, 0, 0, 0, time.UTC)
	mock.ExpectQuery("FROM drift_records WHERE 1=1 AND last_detected_at >= .+ AND last_detected_at <=").
		WithArgs(start, end, 100, 0).
		WillReturnRows(sqlmock.NewRows(driftRecordCols))
	if _, err := r.List(ctx, nil, "", "", 0, 0, &start, &end); err != nil {
		t.Fatalf("date-ranged List: %v", err)
	}
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM drift_records WHERE 1=1 AND severity =`).
		WithArgs("critical").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(4))
	if n, err := r.CountRecords(ctx, nil, "", "critical", nil, nil); err != nil || n != 4 {
		t.Fatalf("CountRecords: %v n=%d", err, n)
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
