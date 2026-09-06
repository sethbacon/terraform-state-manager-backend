package repositories

import (
	"database/sql"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"

	"github.com/terraform-state-manager/terraform-state-manager/internal/testsupport"
)

// ---------------------------------------------------------------------------
// DriftRepository
// ---------------------------------------------------------------------------

func driftRow(token string) *sqlmock.Rows {
	return testsupport.DriftRunRow("d1", "p1", "s1", "app.tfstate", "refs/heads/main", "infra/",
		"completed", 1, 2, 0, true, []byte(`{"resources":[]}`), "", token, "alice",
		"2026-06-10", "2026-06-10", false, 0, 0, false, false, "11111111-1111-4111-8111-111111111111",
		nil, "", "", 0, 0, 0, nil)
}

// driftRowWithBatch is driftRow with a non-NULL batch_id, for the fan-out shape.
func driftRowWithBatch(token, batchID string) *sqlmock.Rows {
	return testsupport.DriftRunRow("d1", "p1", "s1", "app.tfstate", "refs/heads/main", "infra/",
		"dispatched", nil, nil, nil, nil, nil, "", token, "alice",
		"2026-06-10", "2026-06-10", false, 0, 0, false, false, "11111111-1111-4111-8111-111111111111",
		batchID, "", "", 0, 0, 0, nil)
}

func TestDriftRepository_CreateAndGet(t *testing.T) {
	db, mock := newMock(t)
	r := NewDriftRepository(db)

	// The organization is pinned as argument 9 (batch_id, not under test here,
	// follows it as argument 10): sqlmock matches the statement by regex, so
	// without this the column could be dropped from the INSERT entirely and
	// this test would still pass (#436).
	mock.ExpectQuery("INSERT INTO drift_runs").
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), testOrgID, sqlmock.AnyArg()).
		WillReturnRows(driftRow("tok-1"))
	conn := "p1"
	created, err := r.Create(ctx, &DriftRun{PipelineConnectionID: &conn, StateKey: "app.tfstate", Status: "pending", CallbackToken: "tok-1"}, testOrgID)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.Drifted == nil || !*created.Drifted || *created.Added != 1 || *created.Changed != 2 {
		t.Errorf("nullable result fields not mapped: %+v", created)
	}

	mock.ExpectQuery("SELECT .+ FROM drift_runs WHERE id").WithArgs("nope").WillReturnError(sql.ErrNoRows)
	if d, err := r.GetByID(ctx, "nope"); err != nil || d != nil {
		t.Errorf("missing run should be (nil, nil), got %+v %v", d, err)
	}
}

func TestDriftRepository_ListHidesCallbackToken(t *testing.T) {
	db, mock := newMock(t)
	r := NewDriftRepository(db)

	mock.ExpectQuery("SELECT .+ FROM drift_runs WHERE 1=1 ORDER BY created_at DESC LIMIT").WithArgs(50, 0).
		WillReturnRows(driftRow("secret-token"))
	out, err := r.List(ctx, 0, 0, DriftRunFilter{}) // 0 → default limit 50
	if err != nil || len(out) != 1 {
		t.Fatalf("List: %v %d", err, len(out))
	}
	if out[0].CallbackToken != "" {
		t.Error("List leaked the callback token")
	}

	// Out-of-range limits clamp to the default; negative offsets clamp to 0.
	mock.ExpectQuery("SELECT .+ FROM drift_runs WHERE 1=1 ORDER BY created_at DESC LIMIT").WithArgs(50, 0).
		WillReturnRows(driftRow(""))
	if _, err := r.List(ctx, 9999, -3, DriftRunFilter{}); err != nil {
		t.Fatalf("List clamp: %v", err)
	}

	// A status filter binds ahead of the window; CountRuns shares it.
	mock.ExpectQuery(`SELECT .+ FROM drift_runs WHERE 1=1 AND status = \$1 ORDER BY created_at DESC LIMIT`).
		WithArgs("failed", 25, 50).WillReturnRows(driftRow(""))
	if _, err := r.List(ctx, 25, 50, DriftRunFilter{Status: "failed"}); err != nil {
		t.Fatalf("filtered List: %v", err)
	}
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM drift_runs WHERE 1=1 AND status = \$1`).WithArgs("failed").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(7))
	if n, err := r.CountRuns(ctx, DriftRunFilter{Status: "failed"}); err != nil || n != 7 {
		t.Fatalf("CountRuns: %v n=%d", err, n)
	}
}

// TestDriftRepository_Create_SetsBatchID pins the fan-out shape: a run created
// with a BatchID binds it into the INSERT and gets it back on the round trip,
// while a run with none (the legacy/single-target path) binds NULL rather than
// an empty string — which would be a malformed uuid at the database instead of
// the NULL that keeps schedules.last_run_id a real run id for that case.
func TestDriftRepository_Create_SetsBatchID(t *testing.T) {
	db, mock := newMock(t)
	r := NewDriftRepository(db)
	conn := "p1"
	batchID := "22222222-2222-4222-8222-222222222222"

	mock.ExpectQuery("INSERT INTO drift_runs").
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), testOrgID, &batchID).
		WillReturnRows(driftRowWithBatch("tok-1", batchID))
	created, err := r.Create(ctx, &DriftRun{
		PipelineConnectionID: &conn, StateKey: "app.tfstate", Status: "dispatched",
		CallbackToken: "tok-1", BatchID: &batchID,
	}, testOrgID)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.BatchID == nil || *created.BatchID != batchID {
		t.Errorf("BatchID not round-tripped: %+v", created.BatchID)
	}

	mock.ExpectQuery("INSERT INTO drift_runs").
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), testOrgID, (*string)(nil)).
		WillReturnRows(driftRow("tok-2"))
	unfanned, err := r.Create(ctx, &DriftRun{
		PipelineConnectionID: &conn, StateKey: "app.tfstate", Status: "dispatched", CallbackToken: "tok-2",
	}, testOrgID)
	if err != nil {
		t.Fatalf("Create (no batch): %v", err)
	}
	if unfanned.BatchID != nil {
		t.Errorf("an unfanned run must bind NULL batch_id, got %+v", *unfanned.BatchID)
	}
}

// TestDriftList_FilterBatchOrRunID pins the (batch_id = $n OR id = $n) shape: a
// caller filtering by batch id does not know in advance whether the schedule
// that produced it fanned out, so the predicate must match either column.
func TestDriftList_FilterBatchOrRunID(t *testing.T) {
	db, mock := newMock(t)
	r := NewDriftRepository(db)

	mock.ExpectQuery(`SELECT .+ FROM drift_runs WHERE 1=1 AND \(batch_id = \$1 OR id = \$1\) ORDER BY created_at DESC LIMIT`).
		WithArgs("b1", 50, 0).WillReturnRows(driftRow(""))
	if out, err := r.List(ctx, 0, 0, DriftRunFilter{BatchID: "b1"}); err != nil || len(out) != 1 {
		t.Fatalf("List(BatchID): %v len=%d", err, len(out))
	}

	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM drift_runs WHERE 1=1 AND \(batch_id = \$1 OR id = \$1\)`).
		WithArgs("b1").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(3))
	if n, err := r.CountRuns(ctx, DriftRunFilter{BatchID: "b1"}); err != nil || n != 3 {
		t.Fatalf("CountRuns(BatchID): %v n=%d", err, n)
	}
}

// TestDriftList_FilterSourceState pins source_id/state_key filtering, both
// bound after batch_id in the same builder.
func TestDriftList_FilterSourceState(t *testing.T) {
	db, mock := newMock(t)
	r := NewDriftRepository(db)

	mock.ExpectQuery(`SELECT .+ FROM drift_runs WHERE 1=1 AND source_id = \$1 AND state_key = \$2 ORDER BY created_at DESC LIMIT`).
		WithArgs("s1", "app.tfstate", 50, 0).WillReturnRows(driftRow(""))
	if _, err := r.List(ctx, 0, 0, DriftRunFilter{SourceID: "s1", StateKey: "app.tfstate"}); err != nil {
		t.Fatalf("List(source+state): %v", err)
	}
}

// TestDriftList_FilterSince pins the Phase 4a time-window filter the drift
// summary's runs_24h breakdown reuses List/CountRuns' shared builder for,
// rather than a bespoke query: "AND created_at >= $n" binds after every other
// filter, same as the rest of driftRunFilterClause.
func TestDriftList_FilterSince(t *testing.T) {
	db, mock := newMock(t)
	r := NewDriftRepository(db)
	since := time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC)

	mock.ExpectQuery(`SELECT .+ FROM drift_runs WHERE 1=1 AND status = \$1 AND created_at >= \$2 ORDER BY created_at DESC LIMIT`).
		WithArgs("completed", since, 50, 0).WillReturnRows(driftRow(""))
	if _, err := r.List(ctx, 0, 0, DriftRunFilter{Status: "completed", Since: &since}); err != nil {
		t.Fatalf("List(Since): %v", err)
	}

	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM drift_runs WHERE 1=1 AND created_at >= \$1`).
		WithArgs(since).WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(2))
	if n, err := r.CountRuns(ctx, DriftRunFilter{Since: &since}); err != nil || n != 2 {
		t.Fatalf("CountRuns(Since): %v n=%d", err, n)
	}
}

// TestLatestPerState_DistinctOn pins the DISTINCT ON (state_key) shape the
// coverage endpoint joins against: only the newest run per state_key, for one
// source, keyed for a Go-side map lookup.
func TestLatestPerState_DistinctOn(t *testing.T) {
	db, mock := newMock(t)
	r := NewDriftRepository(db)

	mock.ExpectQuery(`SELECT DISTINCT ON \(state_key\) .+ FROM drift_runs WHERE source_id = \$1 ORDER BY state_key, created_at DESC`).
		WithArgs("s1").WillReturnRows(driftRow(""))
	out, err := r.LatestPerState(ctx, "s1")
	if err != nil {
		t.Fatalf("LatestPerState: %v", err)
	}
	if len(out) != 1 || out["app.tfstate"].ID != "d1" {
		t.Fatalf("LatestPerState = %+v, want one row keyed by state_key", out)
	}
}

// TestPruneRuns_KeepsNewestPerState mirrors StateEditRepository.PruneBackups'
// window pattern exactly (partition by source_id, state_key; keep floor before
// the age cap) -- deliberately the same shape, not a second pruning idiom.
func TestPruneRuns_KeepsNewestPerState(t *testing.T) {
	db, mock := newMock(t)
	r := NewDriftRepository(db)

	mock.ExpectExec(`DELETE FROM drift_runs r\s+USING \(\s*SELECT id, row_number\(\) OVER \(\s*PARTITION BY source_id, state_key ORDER BY created_at DESC\s*\) AS rn\s+FROM drift_runs\s*\) w\s+WHERE r\.id = w\.id\s+AND w\.rn > \$1\s+AND r\.created_at < now\(\) - make_interval\(secs => \$2\)\s+AND r\.status NOT IN \('dispatched', 'running'\)`).
		WithArgs(20, (90 * 24 * time.Hour).Seconds()).
		WillReturnResult(sqlmock.NewResult(0, 7))
	n, err := r.PruneRuns(ctx, 20, 90*24*time.Hour)
	if err != nil || n != 7 {
		t.Fatalf("PruneRuns: %v n=%d", err, n)
	}
}

// TestPruneRuns_RejectsUnsafeKeep mirrors PruneBackups' own guard: a zero keep
// floor would turn the age cap into a purge that can erase a state's only
// drift-run history.
func TestPruneRuns_RejectsUnsafeKeep(t *testing.T) {
	db, _ := newMock(t)
	r := NewDriftRepository(db)
	if _, err := r.PruneRuns(ctx, 0, time.Hour); err == nil {
		t.Error("PruneRuns must reject a keep floor of 0")
	}
	if _, err := r.PruneRuns(ctx, 1, 0); err == nil {
		t.Error("PruneRuns must reject a non-positive max age")
	}
}

// TestCountRunsIn pins the scheduler pacing helper (Phase 2): a global,
// unscoped count across the given statuses.
func TestCountRunsIn(t *testing.T) {
	db, mock := newMock(t)
	r := NewDriftRepository(db)

	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM drift_runs WHERE status = ANY`).
		WithArgs([]string{"dispatched", "running"}).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(5))
	if n, err := r.CountRunsIn(ctx, []string{"dispatched", "running"}); err != nil || n != 5 {
		t.Fatalf("CountRunsIn: %v n=%d", err, n)
	}
}

// TestSetCIRun_OnlyAffectsGivenBatch pins the (batch_id=$1 OR id=$1) update
// shape SetCIRun shares with the batch/run filter.
func TestSetCIRun_OnlyAffectsGivenBatch(t *testing.T) {
	db, mock := newMock(t)
	r := NewDriftRepository(db)

	mock.ExpectExec(`UPDATE drift_runs SET ci_run_id=\$2, ci_run_url=\$3.+WHERE batch_id=\$1 OR id=\$1`).
		WithArgs("b1", "12345", "https://dev.azure.com/corp/p/_build/results?buildId=12345").
		WillReturnResult(sqlmock.NewResult(0, 3))
	if err := r.SetCIRun(ctx, "b1", "12345", "https://dev.azure.com/corp/p/_build/results?buildId=12345"); err != nil {
		t.Fatalf("SetCIRun: %v", err)
	}
}

// TestFailBatch_OnlyDispatched pins that FailBatch is scoped to
// status='dispatched', so it cannot clobber a sibling run whose own callback
// already completed or failed by some other path.
func TestFailBatch_OnlyDispatched(t *testing.T) {
	db, mock := newMock(t)
	r := NewDriftRepository(db)

	mock.ExpectExec(`UPDATE drift_runs SET status='failed'.+WHERE batch_id=\$1 AND status='dispatched'`).
		WithArgs("b1", "dispatch failed: no agent available").
		WillReturnResult(sqlmock.NewResult(0, 2))
	if err := r.FailBatch(ctx, "b1", "dispatch failed: no agent available"); err != nil {
		t.Fatalf("FailBatch: %v", err)
	}
}

func TestDriftRepository_ConsumeCallbackToken(t *testing.T) {
	db, mock := newMock(t)
	r := NewDriftRepository(db)

	// First consume matches a row → token spent.
	mock.ExpectExec("UPDATE drift_runs SET callback_token=''").WithArgs("d1", "tok-1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	ok, err := r.ConsumeCallbackToken(ctx, "d1", "tok-1")
	if err != nil || !ok {
		t.Fatalf("first consume should succeed: %v %v", ok, err)
	}

	// Replay: no row matches the already-cleared token.
	mock.ExpectExec("UPDATE drift_runs SET callback_token=''").WithArgs("d1", "tok-1").
		WillReturnResult(sqlmock.NewResult(0, 0))
	ok, err = r.ConsumeCallbackToken(ctx, "d1", "tok-1")
	if err != nil || ok {
		t.Errorf("replayed consume must be rejected: %v %v", ok, err)
	}

	// Empty tokens never consume (no query at all).
	ok, err = r.ConsumeCallbackToken(ctx, "d1", "")
	if err != nil || ok {
		t.Errorf("empty token must be rejected without a query: %v %v", ok, err)
	}
}

func TestDriftRepository_Updates(t *testing.T) {
	db, mock := newMock(t)
	r := NewDriftRepository(db)

	mock.ExpectExec("UPDATE drift_runs SET status").WithArgs("d1", "running", "").
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := r.UpdateStatus(ctx, "d1", "running", ""); err != nil {
		t.Errorf("UpdateStatus: %v", err)
	}

	mock.ExpectExec("UPDATE drift_runs").
		WithArgs("d1", "completed", 1, 2, 0, true, `{"x":1}`, "done", false, 0, 0, false, false, 0, 0, 0, nil).
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := r.UpdateResult(ctx, "d1", "completed", 1, 2, 0, true, []byte(`{"x":1}`), "done", Completeness{}, InfraDrift{}); err != nil {
		t.Errorf("UpdateResult: %v", err)
	}
}

// TestDriftRepository_UpdateResultPersistsCompletenessMarkers is the storage half
// of the run round trip, and the twin of
// TestDriftRecordRepository_PersistsCompletenessMarkers one layer down: the five
// markers must bind into the UPDATE and come back off a scanned row. A run row
// that stores only counts cannot tell a check that verified clean from one that
// never finished — both are added=changed=destroyed=0 — and for a clean or
// unparseable run there is no record to recover the answer from.
func TestDriftRepository_UpdateResultPersistsCompletenessMarkers(t *testing.T) {
	db, mock := newMock(t)
	r := NewDriftRepository(db)

	mock.ExpectExec("UPDATE drift_runs").
		WithArgs("d1", "completed", 0, 0, 0, false, nil, "", true, 5, 9, true, true, 0, 0, 0, nil).
		WillReturnResult(sqlmock.NewResult(0, 1))
	err := r.UpdateResult(ctx, "d1", "completed", 0, 0, 0, false, nil, "",
		Completeness{Truncated: true, OmittedEntries: 5, OmittedAttrs: 9, Unparseable: true, Unmasked: true}, InfraDrift{})
	if err != nil {
		t.Fatalf("UpdateResult: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("markers must be bound into the UPDATE: %v", err)
	}

	// ...and back out again, so per-run history can actually report them.
	mock.ExpectQuery("SELECT .+ FROM drift_runs WHERE id").WithArgs("d1").
		WillReturnRows(testsupport.DriftRunRow("d1", "p1", "s1", "app.tfstate", "", "", "completed",
			0, 0, 0, false, nil, "", "", "alice", "2026-06-10", "2026-06-10",
			true, 5, 9, true, true, "11111111-1111-4111-8111-111111111111",
			nil, "", "", 0, 0, 0, nil))
	got, err := r.GetByID(ctx, "d1")
	if err != nil || got == nil {
		t.Fatalf("GetByID: %v %+v", err, got)
	}
	if !got.Truncated || got.OmittedEntries != 5 || got.OmittedAttrs != 9 || !got.Unparseable || !got.Unmasked {
		t.Errorf("markers lost in the round trip: %+v", got.Completeness)
	}
}

// TestDriftRepository_UpdateResultPersistsInfraDrift is the run twin of
// TestDriftRecordRepository_PersistsInfraDrift: the contract's second triplet
// (resource_drift, migration 000039) must bind into the UPDATE alongside the
// existing resource_changes counts, not merely be accepted by the InfraDrift
// parameter.
func TestDriftRepository_UpdateResultPersistsInfraDrift(t *testing.T) {
	db, mock := newMock(t)
	r := NewDriftRepository(db)

	mock.ExpectExec("UPDATE drift_runs").
		WithArgs("d1", "completed", 0, 0, 0, false, nil, "", false, 0, 0, false, false,
			3, 1, 0, `[{"address":"aws_instance.hand_edited","actions":["update"]}]`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	err := r.UpdateResult(ctx, "d1", "completed", 0, 0, 0, false, nil, "", Completeness{},
		InfraDrift{Added: 3, Changed: 1, Destroyed: 0,
			Summary: []byte(`[{"address":"aws_instance.hand_edited","actions":["update"]}]`)})
	if err != nil {
		t.Fatalf("UpdateResult: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("infra drift counts must be bound into the UPDATE: %v", err)
	}
}

// TestDriftRepository_GetByID_ReadsInfraDriftNonZero is the READ half of the
// infra-drift round trip (the write half is
// TestDriftRepository_UpdateResultPersistsInfraDrift above): a row carrying
// NON-ZERO drift_added/drift_changed/drift_destroyed/drift_summary must scan
// into DriftRun.DriftAdded/DriftChanged/DriftDestroyed/DriftSummary with those
// exact values. An all-zero fixture would only prove the four columns are in
// the right SELECT position -- it cannot show a real value survives the scan,
// which is exactly the class of bug that shipped silently in the write path
// one step ago. added/changed/destroyed (the unapplied-change triplet) are
// asserted unchanged alongside it, pinning that the two triplets are read
// independently rather than one overwriting the other.
func TestDriftRepository_GetByID_ReadsInfraDriftNonZero(t *testing.T) {
	db, mock := newMock(t)
	r := NewDriftRepository(db)

	mock.ExpectQuery("SELECT .+ FROM drift_runs WHERE id").WithArgs("d1").
		WillReturnRows(testsupport.DriftRunRow("d1", "p1", "s1", "app.tfstate", "", "", "completed",
			1, 0, 0, true, []byte(`{"resource_changes":1}`), "", "", "alice", "2026-06-10", "2026-06-10",
			false, 0, 0, false, false, "11111111-1111-4111-8111-111111111111",
			nil, "", "", 3, 1, 0, `[{"address":"aws_instance.hand_edited","actions":["update"]}]`))

	got, err := r.GetByID(ctx, "d1")
	if err != nil || got == nil {
		t.Fatalf("GetByID: %v %+v", err, got)
	}
	if got.DriftAdded != 3 || got.DriftChanged != 1 || got.DriftDestroyed != 0 {
		t.Fatalf("infra drift counts lost in the round trip: added=%d changed=%d destroyed=%d, want 3/1/0",
			got.DriftAdded, got.DriftChanged, got.DriftDestroyed)
	}
	if string(got.DriftSummary) != `[{"address":"aws_instance.hand_edited","actions":["update"]}]` {
		t.Fatalf("drift_summary lost in the round trip: %s", got.DriftSummary)
	}
	// The pre-existing (resource_changes) triplet must be unaffected by reading
	// the new columns beside it.
	if got.Added == nil || *got.Added != 1 || got.Changed == nil || *got.Changed != 0 {
		t.Fatalf("added/changed/destroyed corrupted by the infra drift read: added=%+v changed=%+v", got.Added, got.Changed)
	}
}

// TestDriftRepository_ZeroInfraDrift_BindsExactlyZeroNotSilentlyDropped is the
// back-compat property on the run side: a callback from a runner that predates
// terraform-drift-contract 1.3.0 supplies a zero-value InfraDrift, and the
// statement must still bind explicit 0/0/0/nil for the four new columns rather
// than omitting them -- proving the change is additive to every argument the
// statement already bound.
func TestDriftRepository_ZeroInfraDrift_BindsExactlyZeroNotSilentlyDropped(t *testing.T) {
	db, mock := newMock(t)
	r := NewDriftRepository(db)

	mock.ExpectExec("UPDATE drift_runs").
		WithArgs("d1", "completed", 1, 2, 0, true, `{"x":1}`, "done", false, 0, 0, false, false,
			0, 0, 0, nil).
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := r.UpdateResult(ctx, "d1", "completed", 1, 2, 0, true, []byte(`{"x":1}`), "done",
		Completeness{}, InfraDrift{}); err != nil {
		t.Fatalf("UpdateResult: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("an unset InfraDrift must still bind explicit zeros/nil: %v", err)
	}
}

// TestDriftRepository_UpdateResultWidensTruncated is the run twin of the record
// path's one-way widening. Both storage paths share MarkTruncation precisely so
// a single callback cannot leave the run row saying the summary was complete
// while its record row says it was bounded.
func TestDriftRepository_UpdateResultWidensTruncated(t *testing.T) {
	db, mock := newMock(t)
	r := NewDriftRepository(db)

	// truncated=false is overruled by the omission count beside it.
	mock.ExpectExec("UPDATE drift_runs").
		WithArgs("d1", "completed", 0, 0, 0, false, nil, "", true, 4, 0, false, false, 0, 0, 0, nil).
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := r.UpdateResult(ctx, "d1", "completed", 0, 0, 0, false, nil, "",
		Completeness{Truncated: false, OmittedEntries: 4}, InfraDrift{}); err != nil {
		t.Fatalf("UpdateResult: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("omissions must imply truncated on the run too: %v", err)
	}
}

// ---------------------------------------------------------------------------
// HealthRepository
// ---------------------------------------------------------------------------

var healthCols = []string{"id", "pipeline_connection_id", "repo_ref", "working_dir", "terraform_version",
	"provider_versions", "module_versions", "registry_host", "status", "init_ok", "plan_ok", "success",
	"summary", "detail", "callback_token", "actor", "created_at", "updated_at", "organization_id"}

func healthRow(token string) *sqlmock.Rows {
	return sqlmock.NewRows(healthCols).
		AddRow("h1", "p1", "refs/heads/main", "infra/", "1.9.5",
			[]byte(`{"aws":"5.0.0"}`), []byte(`{}`), "registry.example.com", "completed", true, true, true,
			[]byte(`{}`), "", token, "alice", "2026-06-10", "2026-06-10", "11111111-1111-4111-8111-111111111111")
}

func TestHealthRepository_CreateListGet(t *testing.T) {
	db, mock := newMock(t)
	r := NewHealthRepository(db)

	mock.ExpectQuery("INSERT INTO health_runs").WillReturnRows(healthRow("tok-1"))
	conn := "p1"
	created, err := r.Create(ctx, &HealthRun{PipelineConnectionID: &conn, Status: "pending", CallbackToken: "tok-1"}, testOrgID)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.TerraformVersion != "1.9.5" {
		t.Errorf("fields not mapped: %+v", created)
	}

	mock.ExpectQuery("SELECT .+ FROM health_runs ORDER BY created_at DESC LIMIT").WithArgs(50, 0).
		WillReturnRows(healthRow("secret"))
	out, err := r.List(ctx, -1, 0, "")
	if err != nil || len(out) != 1 {
		t.Fatalf("List: %v %d", err, len(out))
	}
	if out[0].CallbackToken != "" {
		t.Error("List leaked the callback token")
	}

	// Filtered + windowed list reaches the query as (status, limit, offset).
	mock.ExpectQuery("SELECT .+ FROM health_runs WHERE status = .+ ORDER BY .+ LIMIT .+ OFFSET").
		WithArgs("failed", 10, 20).WillReturnRows(healthRow("secret"))
	if _, err := r.List(ctx, 10, 20, "failed"); err != nil {
		t.Fatalf("filtered List: %v", err)
	}

	// CountRuns totals, optionally by status.
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM health_runs WHERE status =`).WithArgs("failed").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(4))
	if n, err := r.CountRuns(ctx, "failed"); err != nil || n != 4 {
		t.Fatalf("CountRuns: %v %d", err, n)
	}

	mock.ExpectQuery("SELECT .+ FROM health_runs WHERE id").WithArgs("nope").WillReturnError(sql.ErrNoRows)
	if h, err := r.GetByID(ctx, "nope"); err != nil || h != nil {
		t.Errorf("missing run should be (nil, nil), got %+v %v", h, err)
	}
}

func TestHealthRepository_TokenAndUpdates(t *testing.T) {
	db, mock := newMock(t)
	r := NewHealthRepository(db)

	mock.ExpectExec("UPDATE health_runs SET callback_token=''").WithArgs("h1", "tok").
		WillReturnResult(sqlmock.NewResult(0, 1))
	ok, err := r.ConsumeCallbackToken(ctx, "h1", "tok")
	if err != nil || !ok {
		t.Fatalf("consume: %v %v", ok, err)
	}
	if ok, _ := r.ConsumeCallbackToken(ctx, "h1", ""); ok {
		t.Error("empty token must be rejected")
	}

	mock.ExpectExec("UPDATE health_runs SET status").WithArgs("h1", "running", "").
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := r.UpdateStatus(ctx, "h1", "running", ""); err != nil {
		t.Errorf("UpdateStatus: %v", err)
	}

	mock.ExpectExec("UPDATE health_runs").
		WithArgs("h1", "completed", true, true, true, `{}`, "").
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := r.UpdateResult(ctx, "h1", "completed", true, true, true, []byte(`{}`), ""); err != nil {
		t.Errorf("UpdateResult: %v", err)
	}
}
