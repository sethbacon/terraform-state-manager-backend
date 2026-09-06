package api

import (
	"net/http"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"

	"github.com/terraform-state-manager/terraform-state-manager/internal/testsupport"
)

// drift_infra_counts_http_test.go is the HTTP-level round trip for migration
// 000039's four columns (drift_added, drift_changed, drift_destroyed,
// drift_summary on both drift_runs and drift_records): the contract's second
// triplet, computed from a plan's resource_drift, as opposed to the
// added/changed/destroyed columns beside them (a plan's resource_changes).
//
// Both tests below cover the SAME two properties the record- and run-level
// repository tests cover one layer down (drift_record_repository_test.go,
// runs_test.go): the four fields must bind when present, and they must still
// bind explicit zeros/nil -- never be silently omitted -- when a producer sends
// none of them. What these add is the missing layer: proof that the HTTP
// handlers actually decode drift_* out of the request body and thread them
// through, not just that the repositories accept them once handed a Detection
// or an InfraDrift value directly.

// TestRunResults_InfraDriftCountsPersist is the callback half: a dispatched
// run's result posts the new triplet, and it must reach BOTH storage paths
// this one callback writes -- the run row (UpdateResultInScope) and the live
// record it upserts (recordDriftOutcome -> UpsertDetectionInScope) -- with the
// same values, not just one of the two.
func TestRunResults_InfraDriftCountsPersist(t *testing.T) {
	e := newDriftEnv(t)
	e.mock.ExpectQuery("FROM drift_runs WHERE id").WithArgs("d1").WillReturnRows(
		testsupport.DriftRunRow("d1", "p1", "s1", "envs/prod.tfstate", "", "", "dispatched",
			nil, nil, nil, nil, nil, "", "tok1", "alice", "2026-06-11", "2026-06-11",
			false, 0, 0, false, false, "11111111-1111-4111-8111-111111111111",
			nil, "", ""))
	e.mock.ExpectExec("UPDATE drift_runs SET callback_token=''").WithArgs("d1", "tok1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	// The run row: infra counts bound alongside the existing (unapplied-change)
	// counts, which stay 0/0/0 here -- the two triplets are independent.
	e.mock.ExpectExec("UPDATE drift_runs").
		WithArgs("d1", "completed", 0, 0, 0, true, nil, "", false, 0, 0, false, false,
			2, 1, 0, `[{"address":"aws_instance.hand_edited","actions":["update"]}]`,
			[]string{testActingOrg}).
		WillReturnResult(sqlmock.NewResult(0, 1))
	sourceRowFor(e, "s1")
	// The live record this callback upserts: same triplet, same summary.
	e.mock.ExpectQuery("INSERT INTO drift_records").
		WithArgs("s1", "envs/prod.tfstate", "p1", "d1", "run", "warning",
			0, 0, 0, nil, nil, false, 0, 0, false, false,
			2, 1, 0, `[{"address":"aws_instance.hand_edited","actions":["update"]}]`,
			[]string{testActingOrg}).
		WillReturnRows(driftRecRow("r1", "open", "warning"))

	body := `{"status":"completed","added":0,"changed":0,"destroyed":0,"drifted":true,
		"drift_added":2,"drift_changed":1,"drift_destroyed":0,
		"drift_summary":[{"address":"aws_instance.hand_edited","actions":["update"]}]}`
	w := e.doWithHeader(http.MethodPost, "/api/v1/drift/runs/d1/results", body,
		"X-TSM-Callback-Token", "tok1")
	if w.Code != http.StatusOK {
		t.Fatalf("callback: %d (%s)", w.Code, w.Body.String())
	}
	if err := e.mock.ExpectationsWereMet(); err != nil {
		t.Errorf("infra drift counts must reach both the run row and the record: %v", err)
	}
}

// TestIngestDrift_InfraDriftCountsPersist is the push half: a pipeline TSM did
// not dispatch reports the new triplet directly (no raw plan -- the driftingest
// mirror does not compute these fields from resource_drift yet), and it must
// reach the upserted record.
func TestIngestDrift_InfraDriftCountsPersist(t *testing.T) {
	e := newDriftEnv(t)
	sourceRowFor(e, "s1")
	e.mock.ExpectQuery("FROM drift_records WHERE source_id .+ external_ref").WithArgs("s1", "run-99").
		WillReturnRows(sqlmock.NewRows(driftRecCols)) // no replay
	e.mock.ExpectQuery("INSERT INTO drift_records").
		WithArgs("s1", "envs/prod.tfstate", nil, nil, "ingest", "warning",
			0, 0, 0, nil, "run-99", false, 0, 0, false, false,
			4, 0, 1, `[{"address":"aws_s3_bucket.orphan","actions":["delete"]}]`,
			[]string{testActingOrg}).
		WillReturnRows(driftRecRow("r1", "open", "critical"))

	body := `{
		"source_id": "s1", "state_key": "envs/prod.tfstate", "external_ref": "run-99",
		"drift_added": 4, "drift_changed": 0, "drift_destroyed": 1,
		"drift_summary": [{"address":"aws_s3_bucket.orphan","actions":["delete"]}]
	}`
	w := e.do(http.MethodPost, "/api/v1/drift/ingest", body)
	if w.Code != http.StatusOK {
		t.Fatalf("ingest: %d (%s)", w.Code, w.Body.String())
	}
	if err := e.mock.ExpectationsWereMet(); err != nil {
		t.Errorf("infra drift counts must reach the INSERT: %v", err)
	}
}

// TestRunResults_NoDriftFields_BackCompatUnaffected is the back-compat property
// at the HTTP boundary: a callback body shaped exactly like a pre-000039 runner
// -- carrying none of the four drift_* keys -- must still be decoded and stored
// with no error and no behaviour change, binding explicit zeros/nil for the new
// columns rather than diverging from what an old runner already produced.
func TestRunResults_NoDriftFields_BackCompatUnaffected(t *testing.T) {
	e := newDriftEnv(t)
	e.mock.ExpectQuery("FROM drift_runs WHERE id").WithArgs("d1").WillReturnRows(
		testsupport.DriftRunRow("d1", "p1", "s1", "envs/prod.tfstate", "", "", "dispatched",
			nil, nil, nil, nil, nil, "", "tok1", "alice", "2026-06-11", "2026-06-11",
			false, 0, 0, false, false, "11111111-1111-4111-8111-111111111111",
			nil, "", ""))
	e.mock.ExpectExec("UPDATE drift_runs SET callback_token=''").WithArgs("d1", "tok1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	e.mock.ExpectExec("UPDATE drift_runs").
		WithArgs("d1", "completed", 1, 0, 0, true, nil, "", false, 0, 0, false, false,
			0, 0, 0, nil, // no drift_* keys in the body -> explicit zeros/nil, not omitted
			[]string{testActingOrg}).
		WillReturnResult(sqlmock.NewResult(0, 1))
	sourceRowFor(e, "s1")
	e.mock.ExpectQuery("INSERT INTO drift_records").
		WithArgs("s1", "envs/prod.tfstate", "p1", "d1", "run", "warning",
			1, 0, 0, nil, nil, false, 0, 0, false, false,
			0, 0, 0, nil, []string{testActingOrg}).
		WillReturnRows(driftRecRow("r1", "open", "warning"))

	// Exactly the shape a pre-000039 runner posts: token/status/counts/drifted
	// only, no drift_* keys anywhere.
	body := `{"added":1,"changed":0,"destroyed":0,"drifted":true}`
	w := e.doWithHeader(http.MethodPost, "/api/v1/drift/runs/d1/results", body,
		"X-TSM-Callback-Token", "tok1")
	if w.Code != http.StatusOK {
		t.Fatalf("callback: %d (%s)", w.Code, w.Body.String())
	}
	if err := e.mock.ExpectationsWereMet(); err != nil {
		t.Errorf("an old-shaped payload must still round-trip: %v", err)
	}
}
