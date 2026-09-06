package api

import (
	"net/http"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"

	"github.com/terraform-state-manager/terraform-state-manager/internal/config"
	"github.com/terraform-state-manager/terraform-state-manager/internal/tenantscope"
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

// --- Recompute from a submitted plan (raw-plan ingest), added after the
// driftingest mirror gained DriftAdded/DriftChanged/DriftDestroyed/
// DriftSummary (terraform-drift-contract 1.4.0). ---

// newDriftEnvWithAudit is newDriftEnv plus a real (mocked) identity DB, so a
// test can assert on what an audit entry's metadata actually contains rather
// than treating the write as fire-and-forget — the established pattern
// documented on sourcesEnv.idMock and used by newCISourcesEnv. Needed here,
// and only here, because "drifted must stay false for an infra-only plan" has
// no OTHER observable surface on the ingest path: drift_records has no
// `drifted` column (a record's mere existence IS the signal), so the audit
// trail is the one place this endpoint states the claim in words.
func newDriftEnvWithAudit(t *testing.T) *sourcesEnv {
	t.Helper()
	t.Setenv("TSM_ENCRYPTION_KEY", "0123456789abcdef0123456789abcdef")
	db, mock, err := newSQLMock()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	idDB, idMock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New (identity): %v", err)
	}
	t.Cleanup(func() { idDB.Close() })

	cfg := &config.Config{}
	h := NewDriftHandlers(cfg, db, idDB, nil)

	r := gin.New()
	r.Use(func(c *gin.Context) {
		tenantscope.Store(c, tenantscope.Scope{OrgIDs: []string{testActingOrg}})
		c.Next()
	})
	v1 := r.Group("/api/v1")
	v1.POST("/drift/ingest", h.IngestDrift())
	return &sourcesEnv{r: r, mock: mock, idMock: idMock}
}

// onlyResourceDriftPlanBody is the conformance corpus's drift/only-resource-
// drift vector, as an /drift/ingest body: no unapplied changes, one
// hand-edited resource. resource_changes is an explicit empty array (not
// absent), matching Summarize's nil-vs-empty distinction -- an absent key
// would report Unparseable instead of a clean, driftful plan.
const onlyResourceDriftPlanBody = `{"source_id":"s1","state_key":"k","plan":{
	"resource_changes": [],
	"resource_drift": [
		{"address":"aws_instance.hand_edited","change":{"actions":["create"],"before":null,"after":{"instance_type":"t3.micro"}}}
	]
}}`

// TestIngestDrift_RawPlan_RecomputesInfraDriftOnly is the ingest half of
// Phase 5's "done when": a plan whose only changes are in resource_drift must
// report drifted:false, drift_added>0 end to end. added/changed/destroyed
// stay 0 (nothing unapplied), drift_added/summary are recomputed from the
// plan via driftingest.Summarize (never reimplemented here), the record is
// still upserted (hasFinding, not resolved clean -- ingest has no run row to
// fall back on), and the audit trail reports drifted:false rather than the
// hardcoded true this branch used to claim unconditionally.
func TestIngestDrift_RawPlan_RecomputesInfraDriftOnly(t *testing.T) {
	e := newDriftEnvWithAudit(t)
	sourceRowFor(e, "s1")
	e.mock.ExpectQuery("INSERT INTO drift_records").
		WithArgs("s1", "k", nil, nil, "ingest", "warning", 0, 0, 0, "[]", nil,
			false, 0, 0, false, false,
			1, 0, 0, `[{"address":"aws_instance.hand_edited","actions":["create"]}]`,
			[]string{testActingOrg}).
		WillReturnRows(driftRecRow("r1", "open", "warning"))
	e.idMock.ExpectQuery("INSERT INTO audit_logs").WithArgs(
		sqlmock.AnyArg(), nil, nil, "drift.ingest", "drift_record", "r1",
		[]byte(`{"drifted":false,"external_ref":"","severity":"warning","state_key":"k"}`),
		sqlmock.AnyArg(), sqlmock.AnyArg(), nil,
	).WillReturnRows(auditInsertReturn())

	w := e.do(http.MethodPost, "/api/v1/drift/ingest", onlyResourceDriftPlanBody)
	if w.Code != http.StatusOK {
		t.Fatalf("ingest: %d (%s)", w.Code, w.Body.String())
	}
	if err := e.mock.ExpectationsWereMet(); err != nil {
		t.Errorf("infra-only drift must still reach the INSERT: %v", err)
	}
	if err := e.idMock.ExpectationsWereMet(); err != nil {
		t.Errorf("the audit trail must report drifted:false for infra-only drift: %v", err)
	}
}

// TestIngestDrift_ExplicitInfraFieldsWinOverPlanRecompute is the precedence
// rule's first direction: a caller that supplies BOTH a raw plan and explicit
// top-level drift_* fields gets the explicit values stored, not the ones
// recomputed from the plan -- the reverse of how added/changed/destroyed
// behave (always recomputed, never honouring a claimed value). The plan here
// would recompute to drift_added=1/drift_changed=0/drift_destroyed=0; the
// explicit fields claim 9/9/9 and must survive.
func TestIngestDrift_ExplicitInfraFieldsWinOverPlanRecompute(t *testing.T) {
	e := newDriftEnv(t)
	sourceRowFor(e, "s1")
	e.mock.ExpectQuery("INSERT INTO drift_records").
		WithArgs("s1", "k", nil, nil, "ingest", "warning", 0, 0, 0, "[]", nil,
			false, 0, 0, false, false,
			9, 9, 9, `[{"address":"explicit.override","actions":["delete"]}]`,
			[]string{testActingOrg}).
		WillReturnRows(driftRecRow("r1", "open", "critical"))

	body := `{"source_id":"s1","state_key":"k",
		"drift_added":9,"drift_changed":9,"drift_destroyed":9,
		"drift_summary":[{"address":"explicit.override","actions":["delete"]}],
		"plan":{
			"resource_changes": [],
			"resource_drift": [
				{"address":"aws_instance.hand_edited","change":{"actions":["create"],"before":null,"after":{"instance_type":"t3.micro"}}}
			]
		}}`
	w := e.do(http.MethodPost, "/api/v1/drift/ingest", body)
	if w.Code != http.StatusOK {
		t.Fatalf("ingest: %d (%s)", w.Code, w.Body.String())
	}
	if err := e.mock.ExpectationsWereMet(); err != nil {
		t.Errorf("explicit top-level drift_* fields must win over the plan recompute: %v", err)
	}
}

// TestIngestDrift_NoExplicitInfraFields_PlanRecomputeApplies is the
// precedence rule's other direction: a caller supplying a raw plan and NO
// top-level drift_* fields gets the plan-derived values -- proving the
// "explicit wins" branch above is a real precedence choice and not simply
// "the plan is ignored."
func TestIngestDrift_NoExplicitInfraFields_PlanRecomputeApplies(t *testing.T) {
	e := newDriftEnv(t)
	sourceRowFor(e, "s1")
	e.mock.ExpectQuery("INSERT INTO drift_records").
		WithArgs("s1", "k", nil, nil, "ingest", "warning", 0, 0, 0, "[]", nil,
			false, 0, 0, false, false,
			1, 0, 0, `[{"address":"aws_instance.hand_edited","actions":["create"]}]`,
			[]string{testActingOrg}).
		WillReturnRows(driftRecRow("r1", "open", "warning"))

	w := e.do(http.MethodPost, "/api/v1/drift/ingest", onlyResourceDriftPlanBody)
	if w.Code != http.StatusOK {
		t.Fatalf("ingest: %d (%s)", w.Code, w.Body.String())
	}
	if err := e.mock.ExpectationsWereMet(); err != nil {
		t.Errorf("with no explicit drift_* fields the plan recompute must apply: %v", err)
	}
}
