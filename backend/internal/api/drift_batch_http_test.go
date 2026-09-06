// drift_batch_http_test.go covers repo-level fan-out dispatch end to end
// through the HTTP handlers (drift-fleet-scale.md Phase 1, task 1.3): the
// golden no-targets shape, the fan-out gate, batch creation, per-run
// callbacks, and the new drift_runs filters/columns.
package api

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"

	"github.com/terraform-state-manager/terraform-state-manager/internal/pipelines"
	"github.com/terraform-state-manager/terraform-state-manager/internal/testsupport"
)

// ---------------------------------------------------------------------------
// Fixtures and fake dispatch servers
// ---------------------------------------------------------------------------

// driftRunInsertRow is a drift_runs RETURNING row for one Create() call inside
// dispatchDriftBatch, parametrized by id/source/state/token/batch so a
// multi-target test can give each item's INSERT a distinct identity.
func driftRunInsertRow(id, sourceID, stateKey, token string, batchID *string) *sqlmock.Rows {
	var batchVal, srcVal any
	if batchID != nil {
		batchVal = *batchID
	}
	if sourceID != "" {
		srcVal = sourceID
	}
	return testsupport.DriftRunRow(id, "p1", srcVal, stateKey, "", "", "dispatched", nil, nil, nil, nil, nil, "", token, "alice",
		"2026-06-11", "2026-06-11", false, 0, 0, false, false, testActingOrg,
		batchVal, "", "")
}

// driftRowWithBatchAndCI is a GetByID/GetByIDInScope RETURNING row carrying a
// non-NULL batch_id and CI run id/link, for GetRun/ListRuns assertions.
func driftRowWithBatchAndCI(id, token, batchID, ciRunID, ciRunURL string) *sqlmock.Rows {
	return testsupport.DriftRunRow(id, "p1", "s1", "app.tfstate", "", "", "completed",
		nil, nil, nil, nil, nil, "", token, "alice", "2026-06-11", "2026-06-11",
		false, 0, 0, false, false, testActingOrg, batchID, ciRunID, ciRunURL)
}

// fakeGitHubDispatch stands in for the GitHub workflow_dispatch endpoint: 204,
// no body, capturing the decoded `inputs` for the caller to assert on.
func fakeGitHubDispatch(t *testing.T, capture *map[string]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Inputs map[string]string `json:"inputs"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if capture != nil {
			*capture = body.Inputs
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// fakeADODispatch stands in for the ADO Pipelines run-creation endpoint,
// capturing templateParameters and responding with a run id + web link built
// from runID.
func fakeADODispatch(t *testing.T, runID int, capture *map[string]string) *httptest.Server {
	t.Helper()
	return fakeADODispatchCapture(t, runID, capture, nil)
}

// fakeADODispatchCapture is fakeADODispatch plus a second capture for the
// Runs API's "variables" bag (drift-fleet-scale.md Phase 1b item 3), for
// tests that assert on the secret-run-variable half of the fan-out wire body.
func fakeADODispatchCapture(t *testing.T, runID int, capture *map[string]string, variables *map[string]pipelines.ADORunVariable) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			TemplateParameters map[string]string                   `json:"templateParameters"`
			Variables          map[string]pipelines.ADORunVariable `json:"variables"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if capture != nil {
			*capture = body.TemplateParameters
		}
		if variables != nil {
			*variables = body.Variables
		}
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"id":%d,"_links":{"web":{"href":"https://dev.azure.com/corp/p/_build/results?buildId=%d"}}}`, runID, runID)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// fakeADORejects stands in for an ADO pipeline that fails every dispatch (e.g.
// a misconfigured connection) without any real terraform ever running.
func fakeADORejects(t *testing.T, status int, message string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, message, status)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// containsArg is a sqlmock.Argument matcher for a string bound argument that
// must CONTAIN a substring, for assertions on an error detail this test does
// not want to pin word-for-word.
type containsArg string

func (a containsArg) Match(v driver.Value) bool {
	s, ok := v.(string)
	return ok && strings.Contains(s, string(a))
}

// ---------------------------------------------------------------------------
// CreateRun: legacy shape, back-compat
// ---------------------------------------------------------------------------

// TestCreateRun_LegacySingle_ResponseKeysUnchangedPlusNullBatchID is the HTTP
// twin of the pipelines-package golden test: a request naming no `targets`
// dispatches exactly one run, gets exactly today's GitHub inputs (no
// "targets" key), and its response is a single DriftRun object (not the
// {batch_id,runs} shape) carrying batch_id: null.
func TestCreateRun_LegacySingle_ResponseKeysUnchangedPlusNullBatchID(t *testing.T) {
	e := newDriftEnv(t)
	var gotInputs map[string]string
	srv := fakeGitHubDispatch(t, &gotInputs)
	defer pipelines.OverrideBaseURLsForTest("", srv.URL)()

	e.mock.ExpectQuery("FROM pipeline_connections WHERE organization_id = ANY").
		WithArgs([]string{testActingOrg}, "p1").
		WillReturnRows(pipelineHTTPRow(t, "github_actions", "ghp_x", map[string]any{"owner": "o", "repo": "r", "workflow_id": "w.yml"}))
	e.mock.ExpectQuery("FROM state_sources WHERE organization_id = ANY").
		WithArgs([]string{testActingOrg}, "s1").
		WillReturnRows(sqlmock.NewRows(apiSourceCols).
			AddRow("s1", "estate", "local", "", []byte(`{"base_path":"/tmp"}`), []byte(`{}`), nil, "2026-06-11", "2026-06-11", testActingOrg))
	e.mock.ExpectQuery("INSERT INTO drift_runs").
		WillReturnRows(driftRunInsertRow("d1", "s1", "app.tfstate", "tok-1", nil))

	w := e.do(http.MethodPost, "/api/v1/drift/runs",
		`{"pipeline_connection_id":"p1","source_id":"s1","state_key":"app.tfstate","working_dir":"infra/"}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (%s)", w.Code, w.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	if _, isBatchShape := got["runs"]; isBatchShape {
		t.Fatalf("a single-target request must return a single run, not {batch_id,runs}: %s", w.Body.String())
	}
	if id, _ := got["id"].(string); id != "d1" {
		t.Errorf("response id = %v, want d1: %s", got["id"], w.Body.String())
	}
	if bid, hasKey := got["batch_id"]; !hasKey || bid != nil {
		t.Errorf("batch_id = %v (present=%v), want null", bid, hasKey)
	}
	if len(gotInputs) != 3 || gotInputs["callback_url"] == "" || gotInputs["callback_token"] == "" || gotInputs["working_dir"] != "infra/" {
		t.Errorf("workflow_dispatch inputs = %v, want EXACTLY the 3 legacy keys", gotInputs)
	}
	if _, hasTargets := gotInputs["targets"]; hasTargets {
		t.Error("a no-targets request must not send a targets input")
	}
}

// TestCreateRun_TargetsSingleItem_FanOutFalse_Allowed_NullBatchID pins that a
// ONE-item `targets` array is the legacy path by definition: it dispatches
// even though the connection never opted into fan_out, and produces the same
// null-batch_id single-run shape.
func TestCreateRun_TargetsSingleItem_FanOutFalse_Allowed_NullBatchID(t *testing.T) {
	e := newDriftEnv(t)
	var gotInputs map[string]string
	srv := fakeGitHubDispatch(t, &gotInputs)
	defer pipelines.OverrideBaseURLsForTest("", srv.URL)()

	e.mock.ExpectQuery("FROM pipeline_connections WHERE organization_id = ANY").
		WithArgs([]string{testActingOrg}, "p1").
		WillReturnRows(pipelineHTTPRow(t, "github_actions", "ghp_x", map[string]any{"owner": "o", "repo": "r", "workflow_id": "w.yml"}))
	e.mock.ExpectQuery("FROM state_sources WHERE organization_id = ANY").
		WithArgs([]string{testActingOrg}, "s1").
		WillReturnRows(sqlmock.NewRows(apiSourceCols).
			AddRow("s1", "estate", "local", "", []byte(`{"base_path":"/tmp"}`), []byte(`{}`), nil, "2026-06-11", "2026-06-11", testActingOrg))
	e.mock.ExpectQuery("INSERT INTO drift_runs").
		WillReturnRows(driftRunInsertRow("d1", "s1", "app.tfstate", "tok-1", nil))

	w := e.do(http.MethodPost, "/api/v1/drift/runs",
		`{"pipeline_connection_id":"p1","targets":[{"source_id":"s1","state_key":"app.tfstate","working_dir":"infra/"}]}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (%s)", w.Code, w.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	if _, isBatchShape := got["runs"]; isBatchShape {
		t.Fatalf("one-item targets must still be the single-run shape: %s", w.Body.String())
	}
	if bid, hasKey := got["batch_id"]; !hasKey || bid != nil {
		t.Errorf("batch_id = %v (present=%v), want null", bid, hasKey)
	}
	if _, hasTargets := gotInputs["targets"]; hasTargets {
		t.Error("a one-item targets request must not send a targets input either")
	}
}

// ---------------------------------------------------------------------------
// CreateRun: validation, before any dispatch or DB call
// ---------------------------------------------------------------------------

// TestCreateRun_Targets_DuplicateSourceStateKey_400 and
// TestCreateRun_Targets_Over100_400 pin that validateDriftTargets runs BEFORE
// actingOrganization/dispatch -- no sqlmock expectation is queued at all, so a
// stray statement fails the test via ExpectationsWereMet.
func TestCreateRun_Targets_DuplicateSourceStateKey_400(t *testing.T) {
	e := newDriftEnv(t)
	body := `{"pipeline_connection_id":"p1","targets":[
		{"source_id":"s1","state_key":"app.tfstate","working_dir":"a/"},
		{"source_id":"s1","state_key":"app.tfstate","working_dir":"b/"}
	]}`
	w := e.do(http.MethodPost, "/api/v1/drift/runs", body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (%s)", w.Code, w.Body.String())
	}
	if err := e.mock.ExpectationsWereMet(); err != nil {
		t.Errorf("validation must refuse before any statement: %v", err)
	}
}

func TestCreateRun_Targets_Over100_400(t *testing.T) {
	e := newDriftEnv(t)
	items := make([]string, 101)
	for i := range items {
		items[i] = fmt.Sprintf(`{"source_id":"s1","state_key":"app%d.tfstate","working_dir":"app%d/"}`, i, i)
	}
	body := `{"pipeline_connection_id":"p1","targets":[` + strings.Join(items, ",") + `]}`
	w := e.do(http.MethodPost, "/api/v1/drift/runs", body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (%s)", w.Code, w.Body.String())
	}
	if err := e.mock.ExpectationsWereMet(); err != nil {
		t.Errorf("validation must refuse before any statement: %v", err)
	}
}

// TestCreateRun_TargetsMultiple_SecondItemSourceOwnedElsewhere_404 is the
// fan-out extension of TestDriftDispatch_RefusesATargetSourceOwnedElsewhere
// (dispatch_ownership_test.go): EVERY item's source is scoped under the
// dispatch authority, not just the first -- a batch cannot use an owned first
// target as cover to reach another organization's state through a later one.
// Same uniform 404 as the legacy single-target refusal, and for the same
// reason: a caller must not be able to tell "not yours" from "does not exist".
func TestCreateRun_TargetsMultiple_SecondItemSourceOwnedElsewhere_404(t *testing.T) {
	e := newDriftEnv(t)
	e.mock.ExpectQuery("FROM pipeline_connections WHERE organization_id = ANY").
		WithArgs([]string{testActingOrg}, "p1").
		WillReturnRows(pipelineHTTPRow(t, "azure_devops", "pat", map[string]any{
			"organization": "corp", "project": "P", "pipeline_id": "7", "fan_out": true,
		}))
	e.mock.ExpectQuery("FROM state_sources WHERE organization_id = ANY").
		WithArgs([]string{testActingOrg}, "s1").
		WillReturnRows(sqlmock.NewRows(apiSourceCols).
			AddRow("s1", "app1", "local", "", []byte(`{"base_path":"/tmp"}`), []byte(`{}`), nil, "2026-06-11", "2026-06-11", testActingOrg))
	e.mock.ExpectQuery("FROM state_sources WHERE organization_id = ANY").
		WithArgs([]string{testActingOrg}, "s-other").
		WillReturnRows(sqlmock.NewRows(apiSourceCols)) // owned elsewhere: no row matches

	body := `{"pipeline_connection_id":"p1","targets":[
		{"source_id":"s1","state_key":"app1.tfstate","working_dir":"app1/"},
		{"source_id":"s-other","state_key":"app2.tfstate","working_dir":"app2/"}
	]}`
	w := e.do(http.MethodPost, "/api/v1/drift/runs", body)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (%s)", w.Code, w.Body.String())
	}
	if err := e.mock.ExpectationsWereMet(); err != nil {
		t.Errorf("the refusal must happen before any INSERT: %v", err)
	}
}

// ---------------------------------------------------------------------------
// CreateRun: the fan-out gate
// ---------------------------------------------------------------------------

// TestCreateRun_TargetsMultiple_FanOutFalse_400 pins the gate: the pipeline
// connection is loaded (so a truly missing connection still 404s first), but
// nothing past it runs -- no source lookup, no INSERT, no dispatch -- once 2+
// items meet a connection whose config.fan_out is not true.
func TestCreateRun_TargetsMultiple_FanOutFalse_400(t *testing.T) {
	e := newDriftEnv(t)
	e.mock.ExpectQuery("FROM pipeline_connections WHERE organization_id = ANY").
		WithArgs([]string{testActingOrg}, "p1").
		WillReturnRows(pipelineHTTPRow(t, "azure_devops", "pat", map[string]any{"organization": "corp", "project": "P", "pipeline_id": "7"}))

	body := `{"pipeline_connection_id":"p1","targets":[
		{"source_id":"s1","state_key":"app1.tfstate","working_dir":"app1/"},
		{"source_id":"s2","state_key":"app2.tfstate","working_dir":"app2/"}
	]}`
	w := e.do(http.MethodPost, "/api/v1/drift/runs", body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (%s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "fan-out capable") {
		t.Errorf("body = %s, want the fan-out refusal", w.Body.String())
	}
	if err := e.mock.ExpectationsWereMet(); err != nil {
		t.Errorf("the gate must refuse before any source load or INSERT: %v", err)
	}
}

// ---------------------------------------------------------------------------
// CreateRun: a real fan-out dispatch
// ---------------------------------------------------------------------------

// TestCreateRun_TargetsMultiple_FanOutTrue_202BatchShape_OneDispatch is the
// end-to-end happy path: 2 targets, one ADO dispatch call carrying BOTH the
// legacy 3 params (from item 0) AND "targets" (both items), one row per
// target under a shared batch_id, and the CI run id/link captured from ADO's
// own response. Per drift-fleet-scale.md Phase 1b item 3, each target's
// callback token travels as a secret Runs API VARIABLE keyed by
// FanOutCallbackTokenVariableName -- NOT inside the "targets" JSON, which is
// compiled verbatim into finalYaml (spike 1.0(b)).
func TestCreateRun_TargetsMultiple_FanOutTrue_202BatchShape_OneDispatch(t *testing.T) {
	e := newDriftEnv(t)
	var gotParams map[string]string
	var gotVariables map[string]pipelines.ADORunVariable
	srv := fakeADODispatchCapture(t, 555, &gotParams, &gotVariables)
	defer pipelines.OverrideBaseURLsForTest(srv.URL, "")()

	e.mock.ExpectQuery("FROM pipeline_connections WHERE organization_id = ANY").
		WithArgs([]string{testActingOrg}, "p1").
		WillReturnRows(pipelineHTTPRow(t, "azure_devops", "pat", map[string]any{
			"organization": "corp", "project": "P", "pipeline_id": "7", "fan_out": true,
		}))
	e.mock.ExpectQuery("FROM state_sources WHERE organization_id = ANY").
		WithArgs([]string{testActingOrg}, "s1").
		WillReturnRows(sqlmock.NewRows(apiSourceCols).
			AddRow("s1", "app1", "local", "", []byte(`{"base_path":"/tmp"}`), []byte(`{}`), nil, "2026-06-11", "2026-06-11", testActingOrg))
	e.mock.ExpectQuery("FROM state_sources WHERE organization_id = ANY").
		WithArgs([]string{testActingOrg}, "s2").
		WillReturnRows(sqlmock.NewRows(apiSourceCols).
			AddRow("s2", "app2", "local", "", []byte(`{"base_path":"/tmp"}`), []byte(`{}`), nil, "2026-06-11", "2026-06-11", testActingOrg))
	e.mock.ExpectQuery("INSERT INTO drift_runs").
		WillReturnRows(driftRunInsertRow("d1", "s1", "app1.tfstate", "tok-a", nil))
	e.mock.ExpectQuery("INSERT INTO drift_runs").
		WillReturnRows(driftRunInsertRow("d2", "s2", "app2.tfstate", "tok-b", nil))
	e.mock.ExpectExec(`UPDATE drift_runs SET ci_run_id=\$2, ci_run_url=\$3`).
		WithArgs(sqlmock.AnyArg(), "555", "https://dev.azure.com/corp/p/_build/results?buildId=555").
		WillReturnResult(sqlmock.NewResult(0, 2))

	body := `{"pipeline_connection_id":"p1","targets":[
		{"source_id":"s1","state_key":"app1.tfstate","working_dir":"app1/"},
		{"source_id":"s2","state_key":"app2.tfstate","working_dir":"app2/"}
	]}`
	w := e.do(http.MethodPost, "/api/v1/drift/runs", body)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (%s)", w.Code, w.Body.String())
	}
	if err := e.mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("SetCIRun must run exactly once for the whole batch: %v", err)
	}

	// The wire body: legacy 3 (from item 0) + targets (both items).
	if gotParams["callback_token"] == "" || gotParams["working_dir"] != "app1/" {
		t.Errorf("legacy params (item 0) = %v", gotParams)
	}
	var targets []map[string]string
	if err := json.Unmarshal([]byte(gotParams["targets"]), &targets); err != nil {
		t.Fatalf("targets did not decode: %v (%s)", err, gotParams["targets"])
	}
	if len(targets) != 2 {
		t.Fatalf("targets has %d entries, want 2: %v", len(targets), targets)
	}
	for i, want := range []struct{ workingDir, stateKey string }{
		{"app1/", "app1.tfstate"},
		{"app2/", "app2.tfstate"},
	} {
		if targets[i]["working_dir"] != want.workingDir || targets[i]["state_key"] != want.stateKey {
			t.Errorf("targets[%d] = %v, want working_dir=%s state_key=%s", i, targets[i], want.workingDir, want.stateKey)
		}
		if !strings.HasSuffix(targets[i]["callback_url"], "/results") {
			t.Errorf("targets[%d] missing callback_url: %v", i, targets[i])
		}
		if _, hasToken := targets[i]["callback_token"]; hasToken {
			t.Errorf("targets[%d] must NOT carry callback_token -- it is compiled verbatim into finalYaml (spike 1.0(b)); got %v", i, targets[i])
		}
	}

	// The security fix under test: each target's callback token instead
	// travels as its OWN secret Runs API variable, resolved at run time.
	wantVar1, wantVar2 := FanOutCallbackTokenVariableName("app1/"), FanOutCallbackTokenVariableName("app2/")
	v1, ok1 := gotVariables[wantVar1]
	v2, ok2 := gotVariables[wantVar2]
	if !ok1 || !ok2 {
		t.Fatalf("variables = %+v, want keys %q and %q", gotVariables, wantVar1, wantVar2)
	}
	if !v1.IsSecret || !v2.IsSecret {
		t.Errorf("both callback-token variables must be isSecret: %+v", gotVariables)
	}
	if v1.Value == "" || v2.Value == "" {
		t.Errorf("both callback-token variables must carry a value: %+v", gotVariables)
	}
	if v1.Value == v2.Value {
		t.Error("each target must carry its OWN one-shot callback token")
	}

	// The response: {batch_id, runs: [2]}, no callback tokens.
	var got struct {
		BatchID string `json:"batch_id"`
		Runs    []struct {
			ID       string `json:"id"`
			StateKey string `json:"state_key"`
			CIRunID  string `json:"ci_run_id"`
			CIRunURL string `json:"ci_run_url"`
		} `json:"runs"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	if got.BatchID == "" {
		t.Error("batch_id must be set for a 2-target dispatch")
	}
	if len(got.Runs) != 2 || got.Runs[0].ID != "d1" || got.Runs[1].ID != "d2" {
		t.Fatalf("runs = %+v, want [d1, d2]", got.Runs)
	}
	if strings.Contains(w.Body.String(), "tok-a") || strings.Contains(w.Body.String(), "tok-b") {
		t.Error("response leaked a callback token")
	}
}

// TestCreateRun_LegacySingle_ADO_NoVariablesKey is the ADO-provider twin of
// TestCreateRun_LegacySingle_ResponseKeysUnchangedPlusNullBatchID: a
// no-targets (single-item) dispatch through an Azure DevOps connection must
// send NO "variables" key at all -- fanOutVariables is only ever populated
// for len(items) > 1 (drift-fleet-scale.md Phase 1b item 3) -- alongside the
// legacy 3-key templateParameters body the pipelines-package golden test
// already pins.
func TestCreateRun_LegacySingle_ADO_NoVariablesKey(t *testing.T) {
	e := newDriftEnv(t)
	var gotParams map[string]string
	var gotVariables map[string]pipelines.ADORunVariable
	srv := fakeADODispatchCapture(t, 1, &gotParams, &gotVariables)
	defer pipelines.OverrideBaseURLsForTest(srv.URL, "")()

	e.mock.ExpectQuery("FROM pipeline_connections WHERE organization_id = ANY").
		WithArgs([]string{testActingOrg}, "p1").
		WillReturnRows(pipelineHTTPRow(t, "azure_devops", "pat", map[string]any{
			"organization": "corp", "project": "P", "pipeline_id": "7",
		}))
	e.mock.ExpectQuery("FROM state_sources WHERE organization_id = ANY").
		WithArgs([]string{testActingOrg}, "s1").
		WillReturnRows(sqlmock.NewRows(apiSourceCols).
			AddRow("s1", "app1", "local", "", []byte(`{"base_path":"/tmp"}`), []byte(`{}`), nil, "2026-06-11", "2026-06-11", testActingOrg))
	e.mock.ExpectQuery("INSERT INTO drift_runs").
		WillReturnRows(driftRunInsertRow("d1", "s1", "app1.tfstate", "tok-a", nil))
	e.mock.ExpectExec(`UPDATE drift_runs SET ci_run_id=\$2, ci_run_url=\$3`).
		WillReturnResult(sqlmock.NewResult(0, 1))

	body := `{"pipeline_connection_id":"p1","source_id":"s1","state_key":"app1.tfstate","working_dir":"app1/"}`
	w := e.do(http.MethodPost, "/api/v1/drift/runs", body)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (%s)", w.Code, w.Body.String())
	}
	if len(gotParams) != 3 || gotParams["callback_token"] == "" {
		t.Errorf("templateParameters = %v, want EXACTLY the 3 legacy keys", gotParams)
	}
	if len(gotVariables) != 0 {
		t.Errorf("a single-target ADO dispatch must send no variables at all, got: %+v", gotVariables)
	}
}

// TestCreateRun_TargetsMultiple_ParamsPassThrough pins Item A
// (drift-fleet-scale.md Phase 1b item 3): each target's Params travels
// unchanged into its "targets" JSON entry, so the fan-out template's
// ${{ t.params.service_connection }} binding has something to read.
func TestCreateRun_TargetsMultiple_ParamsPassThrough(t *testing.T) {
	e := newDriftEnv(t)
	var gotParams map[string]string
	srv := fakeADODispatchCapture(t, 777, &gotParams, nil)
	defer pipelines.OverrideBaseURLsForTest(srv.URL, "")()

	e.mock.ExpectQuery("FROM pipeline_connections WHERE organization_id = ANY").
		WithArgs([]string{testActingOrg}, "p1").
		WillReturnRows(pipelineHTTPRow(t, "azure_devops", "pat", map[string]any{
			"organization": "corp", "project": "P", "pipeline_id": "7", "fan_out": true,
		}))
	e.mock.ExpectQuery("FROM state_sources WHERE organization_id = ANY").
		WithArgs([]string{testActingOrg}, "s1").
		WillReturnRows(sqlmock.NewRows(apiSourceCols).
			AddRow("s1", "app1", "local", "", []byte(`{"base_path":"/tmp"}`), []byte(`{}`), nil, "2026-06-11", "2026-06-11", testActingOrg))
	e.mock.ExpectQuery("FROM state_sources WHERE organization_id = ANY").
		WithArgs([]string{testActingOrg}, "s2").
		WillReturnRows(sqlmock.NewRows(apiSourceCols).
			AddRow("s2", "app2", "local", "", []byte(`{"base_path":"/tmp"}`), []byte(`{}`), nil, "2026-06-11", "2026-06-11", testActingOrg))
	e.mock.ExpectQuery("INSERT INTO drift_runs").
		WillReturnRows(driftRunInsertRow("d1", "s1", "app1.tfstate", "tok-a", nil))
	e.mock.ExpectQuery("INSERT INTO drift_runs").
		WillReturnRows(driftRunInsertRow("d2", "s2", "app2.tfstate", "tok-b", nil))
	e.mock.ExpectExec(`UPDATE drift_runs SET ci_run_id=\$2, ci_run_url=\$3`).
		WillReturnResult(sqlmock.NewResult(0, 2))

	body := `{"pipeline_connection_id":"p1","targets":[
		{"source_id":"s1","state_key":"app1.tfstate","working_dir":"app1/","params":{"service_connection":"sc-app1"}},
		{"source_id":"s2","state_key":"app2.tfstate","working_dir":"app2/","params":{"service_connection":"sc-app2"}}
	]}`
	w := e.do(http.MethodPost, "/api/v1/drift/runs", body)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (%s)", w.Code, w.Body.String())
	}

	var targets []struct {
		WorkingDir string            `json:"working_dir"`
		Params     map[string]string `json:"params"`
	}
	if err := json.Unmarshal([]byte(gotParams["targets"]), &targets); err != nil {
		t.Fatalf("targets did not decode: %v (%s)", err, gotParams["targets"])
	}
	if len(targets) != 2 {
		t.Fatalf("targets has %d entries, want 2: %v", len(targets), targets)
	}
	if targets[0].Params["service_connection"] != "sc-app1" {
		t.Errorf("targets[0].params = %v, want service_connection=sc-app1", targets[0].Params)
	}
	if targets[1].Params["service_connection"] != "sc-app2" {
		t.Errorf("targets[1].params = %v, want service_connection=sc-app2", targets[1].Params)
	}
}

// TestCreateRun_DispatchFails_AllRunsInBatchFailed: the CI dispatch call
// itself fails (a misconfigured pipeline), after both run rows already exist.
// FailBatch must fail every dispatched row in the batch, and the response must
// report both runs as failed with their tokens stripped.
func TestCreateRun_DispatchFails_AllRunsInBatchFailed(t *testing.T) {
	e := newDriftEnv(t)
	srv := fakeADORejects(t, http.StatusBadRequest, "Unexpected parameter 'targets'")
	defer pipelines.OverrideBaseURLsForTest(srv.URL, "")()

	e.mock.ExpectQuery("FROM pipeline_connections WHERE organization_id = ANY").
		WithArgs([]string{testActingOrg}, "p1").
		WillReturnRows(pipelineHTTPRow(t, "azure_devops", "pat", map[string]any{
			"organization": "corp", "project": "P", "pipeline_id": "7", "fan_out": true,
		}))
	e.mock.ExpectQuery("FROM state_sources WHERE organization_id = ANY").
		WithArgs([]string{testActingOrg}, "s1").
		WillReturnRows(sqlmock.NewRows(apiSourceCols).
			AddRow("s1", "app1", "local", "", []byte(`{"base_path":"/tmp"}`), []byte(`{}`), nil, "2026-06-11", "2026-06-11", testActingOrg))
	e.mock.ExpectQuery("FROM state_sources WHERE organization_id = ANY").
		WithArgs([]string{testActingOrg}, "s2").
		WillReturnRows(sqlmock.NewRows(apiSourceCols).
			AddRow("s2", "app2", "local", "", []byte(`{"base_path":"/tmp"}`), []byte(`{}`), nil, "2026-06-11", "2026-06-11", testActingOrg))
	e.mock.ExpectQuery("INSERT INTO drift_runs").
		WillReturnRows(driftRunInsertRow("d1", "s1", "app1.tfstate", "tok-a", nil))
	e.mock.ExpectQuery("INSERT INTO drift_runs").
		WillReturnRows(driftRunInsertRow("d2", "s2", "app2.tfstate", "tok-b", nil))
	e.mock.ExpectExec(`UPDATE drift_runs SET status='failed'`).
		WithArgs(sqlmock.AnyArg(), containsArg("Unexpected parameter")).
		WillReturnResult(sqlmock.NewResult(0, 2))

	body := `{"pipeline_connection_id":"p1","targets":[
		{"source_id":"s1","state_key":"app1.tfstate","working_dir":"app1/"},
		{"source_id":"s2","state_key":"app2.tfstate","working_dir":"app2/"}
	]}`
	w := e.do(http.MethodPost, "/api/v1/drift/runs", body)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (%s)", w.Code, w.Body.String())
	}
	if err := e.mock.ExpectationsWereMet(); err != nil {
		t.Errorf("FailBatch must run once for the whole batch: %v", err)
	}
	var got struct {
		Runs []struct {
			Status string `json:"status"`
			Detail string `json:"detail"`
		} `json:"runs"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	if len(got.Runs) != 2 {
		t.Fatalf("runs = %+v, want 2", got.Runs)
	}
	for i, r := range got.Runs {
		if r.Status != "failed" || r.Detail == "" {
			t.Errorf("runs[%d] = %+v, want status=failed with a detail", i, r)
		}
	}
	if strings.Contains(w.Body.String(), "tok-a") || strings.Contains(w.Body.String(), "tok-b") {
		t.Error("response leaked a callback token")
	}
}

// TestCreateRun_TargetsJSON_MarshalFailure_FailsWholeBatch pins that a
// failure encoding the "targets" JSON payload -- unreachable in production
// (every driftFanOutTarget field is a validated string), forced here via the
// marshalFanOutTargets seam -- fails the whole batch exactly like a real
// dispatch failure, rather than silently sending a body with no "targets"
// (which would create N runs, dispatch only item 0, and leave N-1 stuck
// "dispatched" until the reconciler expires them at TTL). No fake CI server
// is registered at all: if the implementation regressed to dispatching
// anyway, this test would hang or fail against a real network call instead
// of matching the scripted SQL below.
func TestCreateRun_TargetsJSON_MarshalFailure_FailsWholeBatch(t *testing.T) {
	e := newDriftEnv(t)
	old := marshalFanOutTargets
	marshalFanOutTargets = func(any) ([]byte, error) { return nil, fmt.Errorf("boom") }
	defer func() { marshalFanOutTargets = old }()

	e.mock.ExpectQuery("FROM pipeline_connections WHERE organization_id = ANY").
		WithArgs([]string{testActingOrg}, "p1").
		WillReturnRows(pipelineHTTPRow(t, "azure_devops", "pat", map[string]any{
			"organization": "corp", "project": "P", "pipeline_id": "7", "fan_out": true,
		}))
	e.mock.ExpectQuery("FROM state_sources WHERE organization_id = ANY").
		WithArgs([]string{testActingOrg}, "s1").
		WillReturnRows(sqlmock.NewRows(apiSourceCols).
			AddRow("s1", "app1", "local", "", []byte(`{"base_path":"/tmp"}`), []byte(`{}`), nil, "2026-06-11", "2026-06-11", testActingOrg))
	e.mock.ExpectQuery("FROM state_sources WHERE organization_id = ANY").
		WithArgs([]string{testActingOrg}, "s2").
		WillReturnRows(sqlmock.NewRows(apiSourceCols).
			AddRow("s2", "app2", "local", "", []byte(`{"base_path":"/tmp"}`), []byte(`{}`), nil, "2026-06-11", "2026-06-11", testActingOrg))
	e.mock.ExpectQuery("INSERT INTO drift_runs").
		WillReturnRows(driftRunInsertRow("d1", "s1", "app1.tfstate", "tok-a", nil))
	e.mock.ExpectQuery("INSERT INTO drift_runs").
		WillReturnRows(driftRunInsertRow("d2", "s2", "app2.tfstate", "tok-b", nil))
	e.mock.ExpectExec(`UPDATE drift_runs SET status='failed'`).
		WithArgs(sqlmock.AnyArg(), containsArg("encode fan-out targets")).
		WillReturnResult(sqlmock.NewResult(0, 2))

	body := `{"pipeline_connection_id":"p1","targets":[
		{"source_id":"s1","state_key":"app1.tfstate","working_dir":"app1/"},
		{"source_id":"s2","state_key":"app2.tfstate","working_dir":"app2/"}
	]}`
	w := e.do(http.MethodPost, "/api/v1/drift/runs", body)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (%s)", w.Code, w.Body.String())
	}
	if err := e.mock.ExpectationsWereMet(); err != nil {
		t.Errorf("FailBatch must run once for the whole batch, and no dispatch attempted: %v", err)
	}
	var got struct {
		Runs []struct {
			Status string `json:"status"`
			Detail string `json:"detail"`
		} `json:"runs"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	if len(got.Runs) != 2 {
		t.Fatalf("runs = %+v, want 2", got.Runs)
	}
	for i, r := range got.Runs {
		if r.Status != "failed" || !strings.Contains(r.Detail, "encode fan-out targets") {
			t.Errorf("runs[%d] = %+v, want status=failed with an encode-failure detail", i, r)
		}
	}
	if strings.Contains(w.Body.String(), "tok-a") || strings.Contains(w.Body.String(), "tok-b") {
		t.Error("response leaked a callback token")
	}
}

// TestCreateRun_SetCIRunError_StillAccepted pins that SetCIRun is best-effort:
// a DB error recording the CI run id/link must not fail an otherwise
// successful dispatch. The CI job is already running by that point.
func TestCreateRun_SetCIRunError_StillAccepted(t *testing.T) {
	e := newDriftEnv(t)
	srv := fakeADODispatch(t, 42, nil)
	defer pipelines.OverrideBaseURLsForTest(srv.URL, "")()

	e.mock.ExpectQuery("FROM pipeline_connections WHERE organization_id = ANY").
		WithArgs([]string{testActingOrg}, "p1").
		WillReturnRows(pipelineHTTPRow(t, "azure_devops", "pat", map[string]any{
			"organization": "corp", "project": "P", "pipeline_id": "7",
		}))
	e.mock.ExpectQuery("FROM state_sources WHERE organization_id = ANY").
		WithArgs([]string{testActingOrg}, "s1").
		WillReturnRows(sqlmock.NewRows(apiSourceCols).
			AddRow("s1", "app1", "local", "", []byte(`{"base_path":"/tmp"}`), []byte(`{}`), nil, "2026-06-11", "2026-06-11", testActingOrg))
	e.mock.ExpectQuery("INSERT INTO drift_runs").
		WillReturnRows(driftRunInsertRow("d1", "s1", "app1.tfstate", "tok-a", nil))
	e.mock.ExpectExec(`UPDATE drift_runs SET ci_run_id=\$2, ci_run_url=\$3`).
		WillReturnError(fmt.Errorf("connection reset"))

	body := `{"pipeline_connection_id":"p1","source_id":"s1","state_key":"app1.tfstate","working_dir":"app1/"}`
	w := e.do(http.MethodPost, "/api/v1/drift/runs", body)
	if w.Code != http.StatusAccepted {
		t.Fatalf("SetCIRun failing must not fail the dispatch: status = %d (%s)", w.Code, w.Body.String())
	}
	if err := e.mock.ExpectationsWereMet(); err != nil {
		t.Errorf("SetCIRun must still be attempted: %v", err)
	}
}

// ---------------------------------------------------------------------------
// RunResults: each run in a batch still reports its own independent callback
// ---------------------------------------------------------------------------

// TestRunResults_PerRun_AfterBatchDispatch pins that a batch changes nothing
// about the callback path: two runs sharing a batch_id are still two
// independent rows, each authenticated by its OWN one-shot token and each
// resolving its OWN state's record. Consuming one's token has no bearing on
// the other's.
func TestRunResults_PerRun_AfterBatchDispatch(t *testing.T) {
	e := newDriftEnv(t)
	const batchID = "b-shared"

	for _, run := range []struct{ id, token, stateKey string }{
		{"d1", "tok-a", "app1.tfstate"},
		{"d2", "tok-b", "app2.tfstate"},
	} {
		e.mock.ExpectQuery("FROM drift_runs WHERE id").WithArgs(run.id).
			WillReturnRows(testsupport.DriftRunRow(run.id, "p1", "s1", run.stateKey, "", "", "dispatched",
				nil, nil, nil, nil, nil, "", run.token, "alice", "2026-06-11", "2026-06-11",
				false, 0, 0, false, false, testActingOrg, batchID, "", ""))
		e.mock.ExpectExec("UPDATE drift_runs SET callback_token=''").WithArgs(run.id, run.token).
			WillReturnResult(sqlmock.NewResult(0, 1))
		e.mock.ExpectExec("UPDATE drift_runs").WillReturnResult(sqlmock.NewResult(0, 1))
		sourceRowFor(e, "s1")
		e.mock.ExpectExec("UPDATE drift_records SET status='resolved'").WithArgs("s1", run.stateKey, []string{testActingOrg}).
			WillReturnResult(sqlmock.NewResult(0, 1))

		w := e.doWithHeader(http.MethodPost, "/api/v1/drift/runs/"+run.id+"/results", `{"drifted":false}`,
			"X-TSM-Callback-Token", run.token)
		if w.Code != http.StatusOK {
			t.Fatalf("run %s: status = %d (%s)", run.id, w.Code, w.Body.String())
		}
	}
	if err := e.mock.ExpectationsWereMet(); err != nil {
		t.Errorf("each run's callback must be handled independently: %v", err)
	}
}

// ---------------------------------------------------------------------------
// ListRuns / GetRun: the new columns and filter
// ---------------------------------------------------------------------------

func TestListRuns_FilterBatchIDMatchesBatchOrRunID(t *testing.T) {
	e := newDriftEnv(t)
	e.mock.ExpectQuery(`FROM drift_runs WHERE organization_id = ANY\(\$1::uuid\[\]\) AND \(batch_id = \$2 OR id = \$2\) ORDER BY`).
		WithArgs([]string{testActingOrg}, "b1", 50, 0).
		WillReturnRows(driftRowWithBatchAndCI("d1", "secret", "b1", "", ""))
	e.mock.ExpectQuery(`SELECT COUNT\(\*\) FROM drift_runs WHERE organization_id = ANY\(\$1::uuid\[\]\) AND \(batch_id = \$2 OR id = \$2\)`).
		WithArgs([]string{testActingOrg}, "b1").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))

	w := e.do(http.MethodGet, "/api/v1/drift/runs?batch_id=b1", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"batch_id":"b1"`) {
		t.Errorf("body = %s, want the run's batch_id", w.Body.String())
	}
}

func TestGetRun_ExposesBatchIDNotToken(t *testing.T) {
	e := newDriftEnv(t)
	e.mock.ExpectQuery("FROM drift_runs WHERE organization_id = ANY.+AND id").
		WithArgs([]string{testActingOrg}, "d1").
		WillReturnRows(driftRowWithBatchAndCI("d1", "secret-token", "b-123", "555", "https://ado/run/555"))

	w := e.do(http.MethodGet, "/api/v1/drift/runs/d1", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `"batch_id":"b-123"`) {
		t.Errorf("body = %s, want batch_id", body)
	}
	if !strings.Contains(body, `"ci_run_id":"555"`) || !strings.Contains(body, `"ci_run_url":"https://ado/run/555"`) {
		t.Errorf("body = %s, want ci_run_id/ci_run_url", body)
	}
	if strings.Contains(body, "secret-token") {
		t.Error("GetRun leaked the callback token")
	}
}

// ---------------------------------------------------------------------------
// CreatePipeline: fan_out write-time validation
// ---------------------------------------------------------------------------

func TestCreatePipeline_FanOutMustBeBoolean_400(t *testing.T) {
	e := newDriftEnv(t)
	w := e.do(http.MethodPost, "/api/v1/pipelines",
		`{"name":"x","provider":"azure_devops","config":{"fan_out":"yes"}}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (%s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "fan_out must be a boolean") {
		t.Errorf("body = %s", w.Body.String())
	}
	if err := e.mock.ExpectationsWereMet(); err != nil {
		t.Errorf("a bad fan_out must refuse before any statement: %v", err)
	}

	// A real boolean is accepted and reaches the INSERT.
	e.mock.ExpectQuery("INSERT INTO pipeline_connections").
		WillReturnRows(pipelineHTTPRow(t, "azure_devops", "", map[string]any{"organization": "o", "project": "p", "pipeline_id": "1", "fan_out": true}))
	w2 := e.do(http.MethodPost, "/api/v1/pipelines",
		`{"name":"x","provider":"azure_devops","config":{"organization":"o","project":"p","pipeline_id":"1","fan_out":true}}`)
	if w2.Code != http.StatusCreated {
		t.Errorf("a boolean fan_out must be accepted: status = %d (%s)", w2.Code, w2.Body.String())
	}
}

// A database failure part-way through creating a fan-out batch must not strand
// the rows already created: they hold live one-shot tokens that no CI job will
// ever use (nothing has been dispatched yet), so they are failed forward
// exactly like a dispatch failure instead of sitting "dispatched" until the
// reconciler's TTL.
func TestCreateRun_MidBatchCreateFails_FailsCreatedRows(t *testing.T) {
	e := newDriftEnv(t)
	e.mock.ExpectQuery("FROM pipeline_connections WHERE organization_id = ANY").
		WithArgs([]string{testActingOrg}, "p1").
		WillReturnRows(pipelineHTTPRow(t, "azure_devops", "pat", map[string]any{
			"organization": "corp", "project": "P", "pipeline_id": "7", "fan_out": true,
		}))
	e.mock.ExpectQuery("FROM state_sources WHERE organization_id = ANY").
		WithArgs([]string{testActingOrg}, "s1").
		WillReturnRows(sqlmock.NewRows(apiSourceCols).
			AddRow("s1", "app1", "local", "", []byte(`{"base_path":"/tmp"}`), []byte(`{}`), nil, "2026-06-11", "2026-06-11", testActingOrg))
	e.mock.ExpectQuery("FROM state_sources WHERE organization_id = ANY").
		WithArgs([]string{testActingOrg}, "s2").
		WillReturnRows(sqlmock.NewRows(apiSourceCols).
			AddRow("s2", "app2", "local", "", []byte(`{"base_path":"/tmp"}`), []byte(`{}`), nil, "2026-06-11", "2026-06-11", testActingOrg))
	e.mock.ExpectQuery("INSERT INTO drift_runs").
		WillReturnRows(driftRunInsertRow("d1", "s1", "app1.tfstate", "tok-a", nil))
	e.mock.ExpectQuery("INSERT INTO drift_runs").
		WillReturnError(fmt.Errorf("connection reset by peer"))
	e.mock.ExpectExec(`UPDATE drift_runs SET status='failed'`).
		WithArgs(sqlmock.AnyArg(), containsArg("batch aborted")).
		WillReturnResult(sqlmock.NewResult(0, 1))

	body := `{"pipeline_connection_id":"p1","targets":[
		{"source_id":"s1","state_key":"app1.tfstate","working_dir":"app1/"},
		{"source_id":"s2","state_key":"app2.tfstate","working_dir":"app2/"}
	]}`
	w := e.do(http.MethodPost, "/api/v1/drift/runs", body)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (%s)", w.Code, w.Body.String())
	}
	if err := e.mock.ExpectationsWereMet(); err != nil {
		t.Errorf("the row created before the failure must be failed forward: %v", err)
	}
}
