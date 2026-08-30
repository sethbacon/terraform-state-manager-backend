package api

import (
	"net/http"
	"strings"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
)

// The record-level twins of these tests live in drift_markers_http_test.go.
// They pin that a drift RECORD can answer "was this check complete?". These pin
// the same thing one layer up, on the RUN — because for a large class of runs
// the record cannot answer it at all:
//
//   - A clean run writes no record. ResolveClean closes the live finding (or
//     there was none) and stores nothing, so a clean-but-bounded check leaves no
//     marker anywhere unless the run row keeps it.
//   - An unparseable run deliberately touches no record at all (that is the
//     fail-open #382 closed), so the run row is the ONLY place the fact that
//     nothing was verified can be written down.
//   - Records are overwritten in place on re-detection, so even when a record
//     exists it describes the LATEST observation, never run N-3.
//
// Per-run history therefore has to store its own completeness; it cannot derive
// it. These tests are the round trip: markers in on the callback, out again on
// the stored run.

// driftRunRowMarked builds a drift_runs row carrying the five markers, mirroring
// driftRecRowMarked on the record side. The counts are the same on every row —
// the markers are what these tests vary.
func driftRunRowMarked(status, token string, truncated bool, omittedEntries, omittedAttrs int, unparseable, unmasked bool) *sqlmock.Rows {
	return sqlmock.NewRows(driftCols).
		AddRow("d1", "p1", "s1", "envs/prod.tfstate", "", "", status,
			0, 0, 0, false, nil, "", token, "alice", "2026-06-11", "2026-06-11",
			truncated, omittedEntries, omittedAttrs, unparseable, unmasked, "11111111-1111-4111-8111-111111111111")
}

// TestRunResults_UnparseableCleanRunRoundTripsMarkersOnTheRun is the headline
// case. Zero counts from a document the producer could not read are ignorance,
// not a clean result — and this run writes NO record, so if the run row drops
// the markers the fact is lost outright and the run reads as "verified clean".
//
// The round trip is the assertion: the markers must bind into the UPDATE, and
// come back out of the stored run on a subsequent read.
func TestRunResults_UnparseableCleanRunRoundTripsMarkersOnTheRun(t *testing.T) {
	e := newDriftEnv(t)
	e.mock.ExpectQuery("FROM drift_runs WHERE id").WithArgs("d1").
		WillReturnRows(driftRunRowMarked("dispatched", "tok1", false, 0, 0, false, false))
	e.mock.ExpectExec("UPDATE drift_runs SET callback_token=''").WithArgs("d1", "tok1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	// The five markers must reach the run UPDATE, not stop at the handler.
	e.mock.ExpectExec("UPDATE drift_runs").
		WithArgs("d1", "completed", 0, 0, 0, false, nil, "gh run 9",
			false, 0, 0, true, false, []string{testActingOrg}).
		WillReturnResult(sqlmock.NewResult(0, 1))
	sourceRowFor(e, "s1")

	body := `{"status":"completed","added":0,"changed":0,"destroyed":0,"drifted":false,
		"unparseable":true,"unmasked":false,"truncated":false,
		"omitted_entries":0,"omitted_attrs":0,"detail":"gh run 9"}`
	w := e.doWithHeader(http.MethodPost, "/api/v1/drift/runs/d1/results", body,
		"X-TSM-Callback-Token", "tok1")
	if w.Code != http.StatusOK {
		t.Fatalf("callback: %d (%s)", w.Code, w.Body.String())
	}

	// ...and come back on the stored run: per-run history must be able to say
	// this run never finished checking.
	e.mock.ExpectQuery("FROM drift_runs WHERE organization_id = ANY.+AND id").
		WithArgs([]string{testActingOrg}, "d1").
		WillReturnRows(driftRunRowMarked("completed", "", false, 0, 0, true, false))
	w = e.do(http.MethodGet, "/api/v1/drift/runs/d1", "")
	if w.Code != http.StatusOK {
		t.Fatalf("get run: %d (%s)", w.Code, w.Body.String())
	}
	for _, want := range []string{`"unparseable":true`, `"truncated":false`} {
		if !strings.Contains(w.Body.String(), want) {
			t.Errorf("stored run must report %s; got %s", want, w.Body.String())
		}
	}
	if err := e.mock.ExpectationsWereMet(); err != nil {
		t.Errorf("run markers must reach and return from storage: %v", err)
	}
}

// TestRunResults_CleanButBoundedRunRoundTripsMarkersOnTheRun covers the other
// half a record cannot hold: this run IS clean and DOES resolve the live record,
// but its summary was bounded, so "no more drift" was never established. The
// resolve writes the record; the markers have nowhere to go but the run.
func TestRunResults_CleanButBoundedRunRoundTripsMarkersOnTheRun(t *testing.T) {
	e := newDriftEnv(t)
	e.mock.ExpectQuery("FROM drift_runs WHERE id").WithArgs("d1").
		WillReturnRows(driftRunRowMarked("dispatched", "tok1", false, 0, 0, false, false))
	e.mock.ExpectExec("UPDATE drift_runs SET callback_token=''").WithArgs("d1", "tok1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	e.mock.ExpectExec("UPDATE drift_runs").
		WithArgs("d1", "completed", 0, 0, 0, false, nil, "",
			true, 12, 3, false, true, []string{testActingOrg}).
		WillReturnResult(sqlmock.NewResult(0, 1))
	sourceRowFor(e, "s1")
	// Clean and readable, so the live finding is still resolved — the markers
	// qualify the run, they do not change drift semantics.
	e.mock.ExpectExec("UPDATE drift_records SET status='resolved'").
		WithArgs("s1", "envs/prod.tfstate", []string{testActingOrg}).WillReturnResult(sqlmock.NewResult(0, 1))

	body := `{"added":0,"changed":0,"destroyed":0,
		"truncated":true,"omitted_entries":12,"omitted_attrs":3,
		"unparseable":false,"unmasked":true}`
	w := e.doWithHeader(http.MethodPost, "/api/v1/drift/runs/d1/results", body,
		"X-TSM-Callback-Token", "tok1")
	if w.Code != http.StatusOK {
		t.Fatalf("callback: %d (%s)", w.Code, w.Body.String())
	}
	if err := e.mock.ExpectationsWereMet(); err != nil {
		t.Errorf("a clean-but-bounded run must still carry its markers: %v", err)
	}
}
