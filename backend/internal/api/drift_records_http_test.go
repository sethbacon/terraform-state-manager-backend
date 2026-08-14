package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
)

// sourceRowFor queues a state_sources GetByID row (local type; the ingest path
// only needs existence + name).
func sourceRowFor(e *sourcesEnv, id string) {
	e.mock.ExpectQuery("SELECT .+ FROM state_sources WHERE id").WithArgs(id).
		WillReturnRows(sqlmock.NewRows(
			[]string{"id", "name", "type", "endpoint", "config", "scope", "encrypted_credentials", "created_at", "updated_at"}).
			AddRow(id, "estate", "local", "", []byte(`{"base_path":"/tmp"}`), []byte(`{}`), nil, "2026-06-11", "2026-06-11"))
}

var driftRecCols = []string{"id", "source_id", "state_key", "pipeline_connection_id", "last_run_id",
	"origin", "severity", "added", "changed", "destroyed", "summary", "status", "acknowledged_by",
	"acknowledged_at", "ack_note", "resolved_at", "external_ref", "detections", "first_detected_at",
	"last_detected_at", "truncated", "omitted_entries", "omitted_attrs", "unparseable", "unmasked"}

// driftRecRow is a complete, readable, fully-masked record — the markers all say
// "the check finished". driftRecRowMarked covers the interesting cases.
func driftRecRow(id, status, severity string) *sqlmock.Rows {
	return driftRecRowMarked(id, status, severity, false, 0, 0, false, false)
}

func driftRecRowMarked(id, status, severity string, truncated bool, omittedEntries, omittedAttrs int, unparseable, unmasked bool) *sqlmock.Rows {
	return sqlmock.NewRows(driftRecCols).
		AddRow(id, "s1", "envs/prod.tfstate", nil, nil, "ingest", severity, 1, 1, 1,
			[]byte(`[{"address":"aws_instance.web","actions":["update"]}]`), status,
			"", nil, "", nil, "run-77", 1, "2026-06-11", "2026-06-11",
			truncated, omittedEntries, omittedAttrs, unparseable, unmasked)
}

func TestIngestDrift_ParsesPlanAndCreatesRecord(t *testing.T) {
	e := newDriftEnv(t)
	sourceRowFor(e, "s1")
	e.mock.ExpectQuery("FROM drift_records WHERE source_id .+ external_ref").WithArgs("s1", "run-77").
		WillReturnRows(sqlmock.NewRows(driftRecCols)) // no replay
	e.mock.ExpectQuery("INSERT INTO drift_records").
		WillReturnRows(driftRecRow("r1", "open", "critical"))

	body := `{
		"source_id": "s1", "state_key": "envs/prod.tfstate", "external_ref": "run-77",
		"plan": {"resource_changes": [
			{"address": "aws_instance.web", "change": {"actions": ["update"]}},
			{"address": "aws_instance.old", "change": {"actions": ["delete"]}}
		]}
	}`
	w := e.do(http.MethodPost, "/api/v1/drift/ingest", body)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"record"`) {
		t.Fatalf("ingest: %d (%s)", w.Code, w.Body.String())
	}
	if err := e.mock.ExpectationsWereMet(); err != nil {
		t.Errorf("plan must be parsed and the record upserted: %v", err)
	}
}

func TestIngestDrift_ReplayIsIdempotent(t *testing.T) {
	e := newDriftEnv(t)
	sourceRowFor(e, "s1")
	e.mock.ExpectQuery("FROM drift_records WHERE source_id .+ external_ref").WithArgs("s1", "run-77").
		WillReturnRows(driftRecRow("r1", "open", "warning"))

	w := e.do(http.MethodPost, "/api/v1/drift/ingest",
		`{"source_id":"s1","state_key":"envs/prod.tfstate","external_ref":"run-77","added":1}`)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"replay":true`) {
		t.Fatalf("replay: %d (%s)", w.Code, w.Body.String())
	}
	if err := e.mock.ExpectationsWereMet(); err != nil {
		t.Errorf("replay must not insert: %v", err)
	}
}

func TestIngestDrift_CleanResolves(t *testing.T) {
	e := newDriftEnv(t)
	sourceRowFor(e, "s1")
	e.mock.ExpectExec("UPDATE drift_records SET status='resolved'").WithArgs("s1", "envs/prod.tfstate").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// A no-op plan is a clean signal: the live record auto-resolves.
	w := e.do(http.MethodPost, "/api/v1/drift/ingest",
		`{"source_id":"s1","state_key":"envs/prod.tfstate","plan":{"resource_changes":[{"address":"a.b","change":{"actions":["no-op"]}}]}}`)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"resolved":true`) {
		t.Fatalf("clean ingest: %d (%s)", w.Code, w.Body.String())
	}
}

func TestIngestDrift_Validation(t *testing.T) {
	e := newDriftEnv(t)

	if w := e.do(http.MethodPost, "/api/v1/drift/ingest", `{"state_key":"k"}`); w.Code != http.StatusBadRequest {
		t.Errorf("missing source_id: %d", w.Code)
	}
	if w := e.do(http.MethodPost, "/api/v1/drift/ingest", `not json`); w.Code != http.StatusBadRequest {
		t.Errorf("invalid json: %d", w.Code)
	}

	// Unknown source.
	e.mock.ExpectQuery("SELECT .+ FROM state_sources WHERE id").WithArgs("ghost").
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "type", "endpoint", "config", "scope", "encrypted_credentials", "created_at", "updated_at"}))
	if w := e.do(http.MethodPost, "/api/v1/drift/ingest", `{"source_id":"ghost","state_key":"k","added":1}`); w.Code != http.StatusNotFound {
		t.Errorf("unknown source: %d", w.Code)
	}

	// Plan that isn't terraform show -json shaped.
	sourceRowFor(e, "s1")
	if w := e.do(http.MethodPost, "/api/v1/drift/ingest", `{"source_id":"s1","state_key":"k","plan":"not an object"}`); w.Code != http.StatusUnprocessableEntity {
		t.Errorf("bad plan: %d", w.Code)
	}

	// Payloads over the cap are rejected before any DB work.
	huge := `{"source_id":"s1","state_key":"k","plan":{"resource_changes":[` +
		strings.Repeat(`{"address":"a.b","change":{"actions":["update"]}},`, 120000) +
		`{"address":"z.z","change":{"actions":["update"]}}]}}`
	if w := e.do(http.MethodPost, "/api/v1/drift/ingest", huge); w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized plan: %d", w.Code)
	}
}

func TestRunResults_DriftCreatesRecord_CleanResolves(t *testing.T) {
	e := newDriftEnv(t)

	// Run rows must carry a source_id — that's the record identity.
	runRow := func(token string) *sqlmock.Rows {
		return sqlmock.NewRows(driftCols).
			AddRow("d1", "p1", "s1", "envs/prod.tfstate", "", "", "dispatched",
				nil, nil, nil, nil, nil, "", token, "alice", "2026-06-11", "2026-06-11")
	}

	// Drifted callback: consume token → store result → upsert record.
	e.mock.ExpectQuery("FROM drift_runs WHERE id").WithArgs("d1").WillReturnRows(runRow("tok1"))
	e.mock.ExpectExec("UPDATE drift_runs SET callback_token=''").WithArgs("d1", "tok1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	e.mock.ExpectExec("UPDATE drift_runs").WillReturnResult(sqlmock.NewResult(0, 1))
	e.mock.ExpectQuery("INSERT INTO drift_records").WillReturnRows(driftRecRow("r1", "open", "warning"))

	w := e.doWithHeader(http.MethodPost, "/api/v1/drift/runs/d1/results",
		`{"added":1,"changed":2,"destroyed":0,"drifted":true,"summary":[{"address":"a.b","actions":["update"]}]}`,
		"X-TSM-Callback-Token", "tok1")
	if w.Code != http.StatusOK {
		t.Fatalf("drifted callback: %d (%s)", w.Code, w.Body.String())
	}
	if err := e.mock.ExpectationsWereMet(); err != nil {
		t.Errorf("drifted callback must upsert a record: %v", err)
	}

	// Clean callback: same pipeline, but the record layer resolves instead.
	e.mock.ExpectQuery("FROM drift_runs WHERE id").WithArgs("d1").WillReturnRows(runRow("tok2"))
	e.mock.ExpectExec("UPDATE drift_runs SET callback_token=''").WithArgs("d1", "tok2").
		WillReturnResult(sqlmock.NewResult(0, 1))
	e.mock.ExpectExec("UPDATE drift_runs").WillReturnResult(sqlmock.NewResult(0, 1))
	e.mock.ExpectExec("UPDATE drift_records SET status='resolved'").WithArgs("s1", "envs/prod.tfstate").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w = e.doWithHeader(http.MethodPost, "/api/v1/drift/runs/d1/results",
		`{"added":0,"changed":0,"destroyed":0,"drifted":false}`,
		"X-TSM-Callback-Token", "tok2")
	if w.Code != http.StatusOK {
		t.Fatalf("clean callback: %d (%s)", w.Code, w.Body.String())
	}
	if err := e.mock.ExpectationsWereMet(); err != nil {
		t.Errorf("clean callback must resolve the record: %v", err)
	}
}

func TestListDriftRecords(t *testing.T) {
	e := newDriftEnv(t)

	e.mock.ExpectQuery("FROM drift_records WHERE 1=1 AND status = ANY").
		WillReturnRows(driftRecRow("r1", "open", "warning"))
	e.mock.ExpectQuery(`SELECT COUNT\(\*\) FROM drift_records`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	e.mock.ExpectQuery("SELECT status, COUNT").
		WillReturnRows(sqlmock.NewRows([]string{"status", "count"}).AddRow("open", 1))
	w := e.do(http.MethodGet, "/api/v1/drift/records?status=open,acknowledged", "")
	if w.Code != http.StatusOK {
		t.Fatalf("list: %d (%s)", w.Code, w.Body.String())
	}
	var resp struct {
		Records []map[string]any `json:"records"`
		Counts  map[string]int   `json:"counts"`
		Total   int              `json:"total"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil || len(resp.Records) != 1 || resp.Counts["open"] != 1 || resp.Total != 1 {
		t.Errorf("list payload: %v %s", err, w.Body.String())
	}

	if w := e.do(http.MethodGet, "/api/v1/drift/records?status=bogus", ""); w.Code != http.StatusBadRequest {
		t.Errorf("invalid status filter: %d", w.Code)
	}
}

// TestDriftRecordsPagination covers page/per_page windowing plus the optional
// last-detected date range, all bound as query args.
func TestDriftRecordsPagination(t *testing.T) {
	e := newDriftEnv(t)

	// page=2&per_page=25 → LIMIT 25 OFFSET 25; both dates bound as args.
	e.mock.ExpectQuery(`FROM drift_records WHERE 1=1 AND status = ANY.+last_detected_at >=.+last_detected_at <=.+LIMIT.+OFFSET`).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), 25, 25).
		WillReturnRows(driftRecRow("r1", "open", "warning"))
	e.mock.ExpectQuery(`SELECT COUNT\(\*\) FROM drift_records`).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(60))
	e.mock.ExpectQuery("SELECT status, COUNT").
		WillReturnRows(sqlmock.NewRows([]string{"status", "count"}).AddRow("open", 60))
	w := e.do(http.MethodGet,
		"/api/v1/drift/records?status=open&page=2&per_page=25&start_date=2026-07-01T00:00:00Z&end_date=2026-07-09T00:00:00Z", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"total":60`) {
		t.Fatalf("paged records: status = %d (%s)", w.Code, w.Body.String())
	}
	if err := e.mock.ExpectationsWereMet(); err != nil {
		t.Errorf("window and date range must reach the query: %v", err)
	}

	// Out-of-range per_page falls back to the default window; unparsable dates
	// are ignored rather than erroring (mirrors the audit-log filters).
	e.mock.ExpectQuery("FROM drift_records WHERE 1=1 .+ LIMIT .+ OFFSET").
		WithArgs(100, 0).
		WillReturnRows(driftRecRow("r1", "open", "warning"))
	e.mock.ExpectQuery(`SELECT COUNT\(\*\) FROM drift_records`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	e.mock.ExpectQuery("SELECT status, COUNT").
		WillReturnRows(sqlmock.NewRows([]string{"status", "count"}).AddRow("open", 1))
	w = e.do(http.MethodGet, "/api/v1/drift/records?per_page=9999&start_date=not-a-date", "")
	if w.Code != http.StatusOK {
		t.Fatalf("fallback paging: status = %d (%s)", w.Code, w.Body.String())
	}
	if err := e.mock.ExpectationsWereMet(); err != nil {
		t.Errorf("invalid params must fall back to defaults: %v", err)
	}
}

func TestAcknowledgeAndResolveDriftRecord(t *testing.T) {
	e := newDriftEnv(t)

	e.mock.ExpectQuery("UPDATE drift_records").
		WillReturnRows(driftRecRow("r1", "acknowledged", "warning"))
	w := e.do(http.MethodPost, "/api/v1/drift/records/r1/acknowledge", `{"note":"expected during cert rotation"}`)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"acknowledged"`) {
		t.Fatalf("acknowledge: %d (%s)", w.Code, w.Body.String())
	}

	// Acknowledging a non-open record is a conflict, not a 500/404.
	e.mock.ExpectQuery("UPDATE drift_records").WillReturnRows(sqlmock.NewRows(driftRecCols))
	e.mock.ExpectQuery("FROM drift_records WHERE id").WithArgs("r1").
		WillReturnRows(driftRecRow("r1", "resolved", "warning"))
	if w := e.do(http.MethodPost, "/api/v1/drift/records/r1/acknowledge", `{}`); w.Code != http.StatusConflict {
		t.Errorf("ack non-open: %d (%s)", w.Code, w.Body.String())
	}

	// Missing record is a 404.
	e.mock.ExpectQuery("UPDATE drift_records").WillReturnRows(sqlmock.NewRows(driftRecCols))
	e.mock.ExpectQuery("FROM drift_records WHERE id").WithArgs("ghost").
		WillReturnRows(sqlmock.NewRows(driftRecCols))
	if w := e.do(http.MethodPost, "/api/v1/drift/records/ghost/acknowledge", `{}`); w.Code != http.StatusNotFound {
		t.Errorf("ack missing: %d", w.Code)
	}

	// Oversized note rejected up front.
	if w := e.do(http.MethodPost, "/api/v1/drift/records/r1/acknowledge",
		`{"note":"`+strings.Repeat("x", 1001)+`"}`); w.Code != http.StatusBadRequest {
		t.Errorf("oversized note: %d", w.Code)
	}

	// Manual resolve.
	e.mock.ExpectQuery("UPDATE drift_records SET status='resolved'").
		WillReturnRows(driftRecRow("r1", "resolved", "warning"))
	if w := e.do(http.MethodPost, "/api/v1/drift/records/r1/resolve", ""); w.Code != http.StatusOK {
		t.Errorf("resolve: %d (%s)", w.Code, w.Body.String())
	}
}

func TestGetDriftRecord(t *testing.T) {
	e := newDriftEnv(t)

	e.mock.ExpectQuery("FROM drift_records WHERE id").WithArgs("r1").
		WillReturnRows(driftRecRow("r1", "open", "critical"))
	w := e.do(http.MethodGet, "/api/v1/drift/records/r1", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"critical"`) {
		t.Fatalf("get: %d (%s)", w.Code, w.Body.String())
	}

	e.mock.ExpectQuery("FROM drift_records WHERE id").WithArgs("ghost").
		WillReturnRows(sqlmock.NewRows(driftRecCols))
	if w := e.do(http.MethodGet, "/api/v1/drift/records/ghost", ""); w.Code != http.StatusNotFound {
		t.Errorf("missing: %d", w.Code)
	}
}
