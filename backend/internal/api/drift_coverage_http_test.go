// drift_coverage_http_test.go covers the Phase 4a dashboard read-path:
// GET /drift/coverage (per-source join of live states, latest runs, live
// records and schedule membership) and GET /drift/summary (fleet-wide
// aggregates). Both resolve a tenant scope exactly as ListRuns/ListDriftRecords
// do (#393 Phase 3) -- a source, its runs and its records all stay behind the
// SAME organization boundary the rest of the drift plane already enforces.
package api

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"

	"github.com/terraform-state-manager/terraform-state-manager/internal/testsupport"
)

// coverageSourceRow queues a GetByIDInScope row for a local source rooted at
// dir, matching expectSource's shape (sources_http_test.go) but declared here
// so this file does not depend on load order with that test's helper.
func coverageSourceRow(e *sourcesEnv, id, dir string) {
	cfg, _ := json.Marshal(map[string]any{"base_path": dir})
	e.mock.ExpectQuery("SELECT .+ FROM state_sources WHERE organization_id").
		WithArgs(sqlmock.AnyArg(), id).
		WillReturnRows(sqlmock.NewRows(apiSourceCols).
			AddRow(id, "prod", "local", "", cfg, []byte(`{}`), nil, "2026-06-10", "2026-06-10", testActingOrg))
}

// coverageRecordRow is a drift_records row for the coverage join, parametrized
// by state_key (driftRecRow elsewhere in this package fixes it at
// "envs/prod.tfstate", which does not match this file's own state fixtures).
func coverageRecordRow(id, sourceID, stateKey, status, severity string) *sqlmock.Rows {
	return sqlmock.NewRows(driftRecCols).
		AddRow(id, sourceID, stateKey, nil, nil, "run", severity, 1, 0, 0,
			[]byte(`[]`), status, "", nil, "", nil, nil, 1, "2026-06-11", "2026-06-11",
			false, 0, 0, false, false, testActingOrg)
}

// pgNow renders t the way PostgreSQL's `timestamptz::text` cast does (space
// separator, microseconds, 2-digit UTC offset with no colon) -- the format
// drift_repository.go's driftColumns produces and the coverage handler's
// staleness classification must parse back.
func pgNow(t time.Time) string {
	return t.UTC().Format("2006-01-02 15:04:05.999999-07")
}

func TestCoverage_JoinsRunRecordSchedule(t *testing.T) {
	e := newDriftEnv(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "prod.tfstate"), []byte("{}"), 0o600); err != nil {
		t.Fatalf("seed prod.tfstate: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "dev.tfstate"), []byte("{}"), 0o600); err != nil {
		t.Fatalf("seed dev.tfstate: %v", err)
	}

	coverageSourceRow(e, "s1", dir)

	fresh := pgNow(time.Now().Add(-1 * time.Minute))
	e.mock.ExpectQuery(`SELECT DISTINCT ON \(state_key\) .+ FROM drift_runs WHERE organization_id = ANY.+AND source_id = \$2`).
		WithArgs([]string{testActingOrg}, "s1").
		WillReturnRows(testsupport.DriftRunRow("d1", "p1", "s1", "prod.tfstate", "", "", "completed",
			1, 0, 0, true, []byte(`[]`), "", "", "alice", fresh, fresh,
			false, 0, 0, false, false, testActingOrg, nil, "555", "https://ado/build/555"))

	e.mock.ExpectQuery(`FROM drift_records WHERE organization_id = ANY.+AND source_id = \$2.+AND status <> 'resolved'`).
		WithArgs([]string{testActingOrg}, "s1").
		WillReturnRows(coverageRecordRow("r1", "s1", "prod.tfstate", "open", "critical"))

	scheduleCfg, _ := json.Marshal(map[string]any{"pipeline_connection_id": "p1", "source_id": "s1", "state_key": "prod.tfstate"})
	e.mock.ExpectQuery("FROM schedules WHERE organization_id = ANY").
		WithArgs([]string{testActingOrg}).
		WillReturnRows(sqlmock.NewRows(scheduleHTTPCols).
			AddRow("sch1", "nightly", "0 2 * * *", "drift", scheduleCfg, true,
				nil, nil, nil, nil, "2026-06-10", "2026-06-10", testActingOrg))

	w := e.do(http.MethodGet, "/api/v1/drift/coverage?source_id=s1", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", w.Code, w.Body.String())
	}

	var resp struct {
		States []struct {
			Key          string  `json:"key"`
			Scheduled    bool    `json:"scheduled"`
			LastRunID    *string `json:"last_run_id"`
			LastStatus   *string `json:"last_status"`
			Drifted      *bool   `json:"drifted"`
			CIRunURL     *string `json:"ci_run_url"`
			RecordID     *string `json:"record_id"`
			RecordStatus *string `json:"record_status"`
			Severity     *string `json:"severity"`
		} `json:"states"`
		Summary struct {
			Total       int `json:"total"`
			Scheduled   int `json:"scheduled"`
			Unscheduled int `json:"unscheduled"`
			Open        int `json:"open"`
			Critical    int `json:"critical"`
		} `json:"summary"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, w.Body.String())
	}
	if len(resp.States) != 2 {
		t.Fatalf("states = %+v, want 2", resp.States)
	}
	byKey := map[string]int{}
	for i, s := range resp.States {
		byKey[s.Key] = i
	}
	prod := resp.States[byKey["prod.tfstate"]]
	if !prod.Scheduled || prod.LastRunID == nil || *prod.LastRunID != "d1" {
		t.Errorf("prod.tfstate not joined to its run/schedule: %+v", prod)
	}
	if prod.Drifted == nil || !*prod.Drifted {
		t.Errorf("prod.tfstate drifted not carried through: %+v", prod)
	}
	if prod.CIRunURL == nil || *prod.CIRunURL != "https://ado/build/555" {
		t.Errorf("prod.tfstate ci_run_url missing: %+v", prod)
	}
	if prod.RecordID == nil || prod.RecordStatus == nil || *prod.RecordStatus != "open" || prod.Severity == nil || *prod.Severity != "critical" {
		t.Errorf("prod.tfstate live record not joined: %+v", prod)
	}
	dev := resp.States[byKey["dev.tfstate"]]
	if dev.Scheduled || dev.LastRunID != nil || dev.RecordID != nil {
		t.Errorf("dev.tfstate must show no run, no schedule, no record: %+v", dev)
	}
	if resp.Summary.Total != 2 || resp.Summary.Scheduled != 1 || resp.Summary.Unscheduled != 1 ||
		resp.Summary.Open != 1 || resp.Summary.Critical != 1 {
		t.Errorf("summary = %+v, want total=2 scheduled=1 unscheduled=1 open=1 critical=1", resp.Summary)
	}
}

// TestCoverage_StaleAndUnscheduled pins the staleness classification: a run
// well outside stale_after counts as stale even though it exists, and a state
// with no run at all counts as stale too.
func TestCoverage_StaleAndUnscheduled(t *testing.T) {
	e := newDriftEnv(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "old.tfstate"), []byte("{}"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "never.tfstate"), []byte("{}"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	coverageSourceRow(e, "s1", dir)

	old := pgNow(time.Now().Add(-72 * time.Hour))
	e.mock.ExpectQuery(`SELECT DISTINCT ON \(state_key\) .+ FROM drift_runs`).
		WithArgs([]string{testActingOrg}, "s1").
		WillReturnRows(testsupport.DriftRunRow("d1", "p1", "s1", "old.tfstate", "", "", "completed",
			0, 0, 0, false, nil, "", "", "alice", old, old,
			false, 0, 0, false, false, testActingOrg, nil, "", ""))
	e.mock.ExpectQuery(`FROM drift_records WHERE organization_id = ANY.+AND source_id = \$2.+AND status <> 'resolved'`).
		WithArgs([]string{testActingOrg}, "s1").
		WillReturnRows(sqlmock.NewRows(driftRecCols)) // no live records
	e.mock.ExpectQuery("FROM schedules WHERE organization_id = ANY").
		WithArgs([]string{testActingOrg}).
		WillReturnRows(sqlmock.NewRows(scheduleHTTPCols)) // no schedules

	w := e.do(http.MethodGet, "/api/v1/drift/coverage?source_id=s1&stale_after=24h", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", w.Code, w.Body.String())
	}
	var resp struct {
		Summary struct {
			Stale       int `json:"stale"`
			Unscheduled int `json:"unscheduled"`
		} `json:"summary"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Summary.Stale != 2 {
		t.Errorf("summary.stale = %d, want 2 (a 72h-old run and a never-checked state)", resp.Summary.Stale)
	}
	if resp.Summary.Unscheduled != 2 {
		t.Errorf("summary.unscheduled = %d, want 2", resp.Summary.Unscheduled)
	}
}

// TestCoverage_CachesJoinInputsForSameSource pins the 60s per-source cache
// (drift-fleet-scale.md #567 Phase 4a: "coverage is computed, not stored...
// cache per source for 60s"). The source lookup re-verifies scope on EVERY
// request (a deleted/re-owned source must 404 immediately, never serve stale
// cached data past that), but the expensive part -- the connector's state
// listing plus the three repository joins -- is queued only ONCE: a second
// request within the window that tried to redo them would find no matching
// expectation and 500.
func TestCoverage_CachesJoinInputsForSameSource(t *testing.T) {
	e := newDriftEnv(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.tfstate"), []byte("{}"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	coverageSourceRow(e, "s1", dir)
	e.mock.ExpectQuery(`SELECT DISTINCT ON \(state_key\) .+ FROM drift_runs`).
		WithArgs([]string{testActingOrg}, "s1").
		WillReturnRows(sqlmock.NewRows(testsupport.DriftRunColumns))
	e.mock.ExpectQuery(`FROM drift_records WHERE organization_id = ANY.+AND source_id = \$2.+AND status <> 'resolved'`).
		WithArgs([]string{testActingOrg}, "s1").
		WillReturnRows(sqlmock.NewRows(driftRecCols))
	e.mock.ExpectQuery("FROM schedules WHERE organization_id = ANY").
		WithArgs([]string{testActingOrg}).
		WillReturnRows(sqlmock.NewRows(scheduleHTTPCols))
	// The SECOND request's own source re-check -- queued separately from the
	// first, because that half is never cached.
	coverageSourceRow(e, "s1", dir)

	for i := 0; i < 2; i++ {
		w := e.do(http.MethodGet, "/api/v1/drift/coverage?source_id=s1", "")
		if w.Code != http.StatusOK {
			t.Fatalf("request %d: status = %d (%s)", i, w.Code, w.Body.String())
		}
	}
	if err := e.mock.ExpectationsWereMet(); err != nil {
		t.Errorf("both requests together must consume exactly two source lookups and ONE set of joins: %v", err)
	}
}

func TestCoverage_SourceNotFound404(t *testing.T) {
	e := newDriftEnv(t)
	e.mock.ExpectQuery("SELECT .+ FROM state_sources WHERE organization_id").
		WithArgs([]string{testActingOrg}, "nope").
		WillReturnError(sql.ErrNoRows)
	if w := e.do(http.MethodGet, "/api/v1/drift/coverage?source_id=nope", ""); w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (%s)", w.Code, w.Body.String())
	}
}

func TestCoverage_MissingSourceID_400(t *testing.T) {
	e := newDriftEnv(t)
	if w := e.do(http.MethodGet, "/api/v1/drift/coverage", ""); w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (%s)", w.Code, w.Body.String())
	}
}

// TestCoverage_ScopedToOrganization is the mutation-verification target: a
// source that exists but belongs to ANOTHER organization must read back
// EXACTLY as one that does not exist, never as that organization's coverage.
func TestCoverage_ScopedToOrganization(t *testing.T) {
	e := newDriftEnv(t)
	// GetByIDInScope's own WHERE clause is what refuses the row -- the fixture
	// returns no rows for the scoped query, exactly as production PostgreSQL
	// would for a source owned by a different organization_id.
	e.mock.ExpectQuery("SELECT .+ FROM state_sources WHERE organization_id").
		WithArgs([]string{testActingOrg}, "s-other-org").
		WillReturnRows(sqlmock.NewRows(apiSourceCols)) // no row: not in scope
	w := e.do(http.MethodGet, "/api/v1/drift/coverage?source_id=s-other-org", "")
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for a source outside the caller's organization (%s)", w.Code, w.Body.String())
	}
}

func TestDriftSummary_Grouping(t *testing.T) {
	e := newDriftEnv(t)

	e.mock.ExpectQuery(`FROM drift_records r\s+JOIN state_sources s ON s\.id = r\.source_id\s+WHERE r\.status <> 'resolved' AND r\.organization_id = ANY.+AND s\.organization_id = ANY`).
		WithArgs([]string{testActingOrg}).
		WillReturnRows(sqlmock.NewRows([]string{"source_id", "source_name", "open", "acknowledged", "critical"}).
			AddRow("s1", "prod", 2, 1, 1))
	e.mock.ExpectQuery(`SELECT COUNT\(\*\) FROM drift_records WHERE organization_id = ANY.+AND status <> 'resolved' AND \(unparseable OR truncated\)`).
		WithArgs([]string{testActingOrg}).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(3))

	// runs_24h: completed/failed/running/dispatched, each its own scoped+windowed
	// count reusing DriftRunFilter.Since -- four calls rather than one bespoke
	// GROUP BY, so the drift_runs surface gains no new SQL shape for this.
	for _, status := range []string{"completed", "failed", "dispatched", "running"} {
		var n int
		switch status {
		case "completed":
			n = 5
		case "failed":
			n = 1
		case "dispatched":
			n = 2
		case "running":
			n = 1
		}
		e.mock.ExpectQuery(`SELECT COUNT\(\*\) FROM drift_runs WHERE organization_id = ANY.+AND status = \$2.+AND created_at >= \$3`).
			WithArgs([]string{testActingOrg}, status, sqlmock.AnyArg()).
			WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(n))
	}
	// in_flight: current dispatched+running, unwindowed.
	e.mock.ExpectQuery(`SELECT COUNT\(\*\) FROM drift_runs WHERE organization_id = ANY.+AND status = \$2`).
		WithArgs([]string{testActingOrg}, "dispatched").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(2))
	e.mock.ExpectQuery(`SELECT COUNT\(\*\) FROM drift_runs WHERE organization_id = ANY.+AND status = \$2`).
		WithArgs([]string{testActingOrg}, "running").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))

	w := e.do(http.MethodGet, "/api/v1/drift/summary", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", w.Code, w.Body.String())
	}
	var resp struct {
		RecordsBySource []struct {
			SourceID string `json:"source_id"`
			Open     int    `json:"open"`
			Critical int    `json:"critical"`
		} `json:"records_by_source"`
		Runs24h struct {
			Completed  int `json:"completed"`
			Failed     int `json:"failed"`
			Dispatched int `json:"dispatched"`
		} `json:"runs_24h"`
		IncompleteRecords int `json:"incomplete_records"`
		InFlight          int `json:"in_flight"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, w.Body.String())
	}
	if len(resp.RecordsBySource) != 1 || resp.RecordsBySource[0].SourceID != "s1" || resp.RecordsBySource[0].Open != 2 {
		t.Errorf("records_by_source = %+v", resp.RecordsBySource)
	}
	if resp.Runs24h.Completed != 5 || resp.Runs24h.Failed != 1 || resp.Runs24h.Dispatched != 3 {
		t.Errorf("runs_24h = %+v, want completed=5 failed=1 dispatched=3 (dispatched+running folded together)", resp.Runs24h)
	}
	if resp.IncompleteRecords != 3 {
		t.Errorf("incomplete_records = %d, want 3", resp.IncompleteRecords)
	}
	if resp.InFlight != 3 {
		t.Errorf("in_flight = %d, want 3 (2 dispatched + 1 running)", resp.InFlight)
	}
}
