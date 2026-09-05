package repositories

import (
	"errors"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/jackc/pgx/v5/pgconn"
)

var driftRecordCols = []string{"id", "source_id", "state_key", "pipeline_connection_id", "last_run_id",
	"origin", "severity", "added", "changed", "destroyed", "summary", "status", "acknowledged_by",
	"acknowledged_at", "ack_note", "resolved_at", "external_ref", "detections", "first_detected_at",
	"last_detected_at", "truncated", "omitted_entries", "omitted_attrs", "unparseable", "unmasked", "organization_id"}

func driftRecordRow(id, status string) *sqlmock.Rows {
	return sqlmock.NewRows(driftRecordCols).
		AddRow(id, "s1", "app.tfstate", nil, nil, "run", "warning", 1, 2, 0, []byte(`[]`), status,
			"", nil, "", nil, nil, 1, "2026-06-11", "2026-06-11",
			false, 0, 0, false, false, "11111111-1111-4111-8111-111111111111")
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
		WillReturnError(&pgconn.PgError{Code: "23505"})
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
				true, 5, 9, true, true, "11111111-1111-4111-8111-111111111111"))

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
		WithArgs([]string{"open", "acknowledged"}, "s1", 100, 0).
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

// TestLiveByState pins the Phase 4a coverage join: only NON-resolved records for
// one source, keyed by state_key so the handler can look one up per StateRef
// without a second round trip.
func TestLiveByState(t *testing.T) {
	db, mock := newMock(t)
	r := NewDriftRecordRepository(db)

	mock.ExpectQuery(`FROM drift_records WHERE source_id = \$1 AND status <> 'resolved'`).
		WithArgs("s1").WillReturnRows(driftRecordRow("r1", "open"))
	out, err := r.LiveByState(ctx, "s1")
	if err != nil {
		t.Fatalf("LiveByState: %v", err)
	}
	if len(out) != 1 || out["app.tfstate"].ID != "r1" {
		t.Fatalf("LiveByState = %+v, want one row keyed by state_key", out)
	}
}

// TestPruneResolved pins the Phase 4a resolved-record sweep: an age-only
// delete (no keep floor, unlike PruneRuns/PruneBackups) since a resolved
// record has no "current" restore point to protect -- each one is already its
// own closed history entry.
func TestPruneResolved(t *testing.T) {
	db, mock := newMock(t)
	r := NewDriftRecordRepository(db)

	mock.ExpectExec(`DELETE FROM drift_records WHERE status='resolved' AND resolved_at < now\(\) - make_interval\(secs => \$1\)`).
		WithArgs((180 * 24 * time.Hour).Seconds()).
		WillReturnResult(sqlmock.NewResult(0, 4))
	n, err := r.PruneResolved(ctx, 180*24*time.Hour)
	if err != nil || n != 4 {
		t.Fatalf("PruneResolved: %v n=%d", err, n)
	}

	if _, err := r.PruneResolved(ctx, 0); err == nil {
		t.Error("PruneResolved must reject a non-positive max age")
	}
}

// TestCountOpenBySeverity pins the tsm_drift_records_open{severity} gauge's
// query -- the metric Phase 2 deferred and this phase implements.
func TestCountOpenBySeverity(t *testing.T) {
	db, mock := newMock(t)
	r := NewDriftRecordRepository(db)

	mock.ExpectQuery(`SELECT severity, COUNT\(\*\) FROM drift_records WHERE status='open' GROUP BY severity`).
		WillReturnRows(sqlmock.NewRows([]string{"severity", "count"}).AddRow("critical", 2).AddRow("warning", 5))
	counts, err := r.CountOpenBySeverity(ctx)
	if err != nil || counts["critical"] != 2 || counts["warning"] != 5 {
		t.Fatalf("CountOpenBySeverity: %v %+v", err, counts)
	}
}

// TestCountsBySource pins the drift summary's per-source breakdown: only LIVE
// (non-resolved) records are grouped, and the join brings in the source name.
func TestCountsBySource(t *testing.T) {
	db, mock := newMock(t)
	r := NewDriftRecordRepository(db)

	mock.ExpectQuery(`FROM drift_records r\s+JOIN state_sources s ON s\.id = r\.source_id\s+WHERE r\.status <> 'resolved'\s+GROUP BY r\.source_id, s\.name`).
		WillReturnRows(sqlmock.NewRows([]string{"source_id", "source_name", "open", "acknowledged", "critical"}).
			AddRow("s1", "prod", 2, 1, 1))
	out, err := r.CountsBySource(ctx)
	if err != nil || len(out) != 1 || out[0].SourceID != "s1" || out[0].Open != 2 || out[0].Critical != 1 {
		t.Fatalf("CountsBySource: %v %+v", err, out)
	}
}

// TestCountIncomplete pins the drift summary's incomplete_records field: a
// live record whose check did not finish (unparseable or truncated).
func TestCountIncomplete(t *testing.T) {
	db, mock := newMock(t)
	r := NewDriftRecordRepository(db)

	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM drift_records WHERE status <> 'resolved' AND \(unparseable OR truncated\)`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(3))
	n, err := r.CountIncomplete(ctx)
	if err != nil || n != 3 {
		t.Fatalf("CountIncomplete: %v n=%d", err, n)
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
