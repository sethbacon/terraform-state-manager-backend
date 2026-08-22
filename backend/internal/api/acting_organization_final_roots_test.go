package api

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
	idtenantscope "github.com/sethbacon/terraform-suite-identity/identity/tenantscope"
	"github.com/terraform-state-manager/terraform-state-manager/internal/tenantscope"
)

// The last two partition roots that a handler stamps rather than inherits:
// health_runs and state_transfers (#436). Both take the CALLER's acting
// organization, and each has a reason it cannot take anyone else's.
//
// WHAT sqlmock CAN AND CANNOT SEE HERE. It matches a statement by regex and
// returns whatever columns the fixture declares, so it is blind to a projection
// that stopped selecting a column -- that is what the integration tests in the
// shared identity module are for. It is NOT blind to the two things asserted
// below: an ExpectQuery regex that requires `organization_id` fails when the
// INSERT stops naming the column, and WithArgs fails when the wrong value is
// bound. Those are statement text and arguments, which is exactly what a mock
// does observe.

const otherOrg = "22222222-2222-4222-8222-222222222222"

// ---------------------------------------------------------------------------
// health_runs

func TestHealthRun_IsStampedWithTheActingOrganization(t *testing.T) {
	e := newDriftEnv(t)

	e.mock.ExpectQuery("SELECT .+ FROM pipeline_connections WHERE id").WithArgs("p1").
		WillReturnRows(pipelineHTTPRow(t, "github", "tok", map[string]any{"repo": "o/r"}))
	// The regex REQUIRES the column: drop it from the INSERT and no expectation
	// matches, which is the failure this assertion exists to produce.
	e.mock.ExpectQuery(`INSERT INTO health_runs[\s\S]*organization_id`).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), testActingOrg).
		WillReturnRows(healthRow("tok-1"))

	w := e.do(http.MethodPost, "/api/v1/health-lab/runs", `{"pipeline_connection_id":"p1"}`)
	if w.Code != http.StatusCreated && w.Code != http.StatusOK && w.Code != http.StatusBadGateway {
		t.Fatalf("create run: status = %d (%s)", w.Code, w.Body.String())
	}
	if err := e.mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("the INSERT did not name organization_id, or bound the wrong value: %v", err)
	}
}

// TestHealthRun_RefusesAConnectionOwnedElsewhere covers the cross-check. The
// pipeline connection cannot SUPPLY the organization -- health_runs's foreign key
// is ON DELETE SET NULL, so an inherited answer evaporates when the connection is
// deleted -- but it can still contradict the caller, and a contradiction means
// the caller is dispatching work at another tenant's connection.
func TestHealthRun_RefusesAConnectionOwnedElsewhere(t *testing.T) {
	e := newDriftEnv(t)

	rows := pipelineHTTPRow(t, "github", "tok", map[string]any{"repo": "o/r"})
	e.mock.ExpectQuery("SELECT .+ FROM pipeline_connections WHERE id").WithArgs("p1").
		WillReturnRows(rowsOwnedBy(t, rows, otherOrg))
	// No INSERT is expected: the refusal must happen BEFORE the write.

	w := e.do(http.MethodPost, "/api/v1/health-lab/runs", `{"pipeline_connection_id":"p1"}`)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (%s)", w.Code, w.Body.String())
	}
	// 404 and not 403: a caller outside the owning organization must not learn
	// which connection ids exist elsewhere in the fleet.
	if strings.Contains(w.Body.String(), otherOrg) {
		t.Errorf("the response names the owning organization: %s", w.Body.String())
	}
	if err := e.mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("a statement ran that should not have: %v", err)
	}
}

// TestHealthRun_UnresolvedScopeIs500 keeps the distinction that #436 turns on: an
// unresolved scope is a WIRING FAULT, not an empty one. Treating it as "no
// memberships" would make the route silently stop stamping the moment the
// middleware came unwired -- a route that quietly writes unowned rows.
func TestHealthRun_UnresolvedScopeIs500(t *testing.T) {
	e := newDriftEnvWithoutScope(t)

	w := e.do(http.MethodPost, "/api/v1/health-lab/runs", `{"pipeline_connection_id":"p1"}`)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (%s)", w.Code, w.Body.String())
	}
	// The MESSAGE matters, not just the status. An earlier version of this test
	// asserted the status alone and passed for the wrong reason: with no
	// expectations registered, the pipeline lookup ALSO 500s, so the test was
	// green whether or not the scope was ever checked. Naming the route's wiring
	// is what distinguishes the two.
	if !strings.Contains(w.Body.String(), "tenant scope was not resolved") {
		t.Fatalf("500 for the wrong reason: %s -- the request reached the database "+
			"instead of being refused for an unresolved scope", w.Body.String())
	}
	// And exactly ONE response. A handler that writes the refusal and then carries
	// on writes a second body after it, which gin appends -- so the assertion
	// above still finds its substring and passes while the request keeps running.
	// That is how this test previously survived having the return removed. Two
	// concatenated JSON objects do not unmarshal, so this is what actually proves
	// the handler stopped.
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("the handler wrote more than one response (%s): the refusal did not "+
			"return, so the request continued past it", w.Body.String())
	}
}

// TestHealthRun_UnstampedConnectionTakesTheCallersOrganization separates two
// values that the happy path cannot tell apart.
//
// Everywhere else the connection's organization and the caller's are the same, so
// a handler that stamped conn.OrganizationID would look identical to one that
// stamped the caller's. Here the connection carries NO organization -- the state
// #436's backfill is still repairing -- and the row must still be owned by the
// caller rather than by NULL.
func TestHealthRun_UnstampedConnectionTakesTheCallersOrganization(t *testing.T) {
	e := newDriftEnv(t)

	cfgJSON, _ := json.Marshal(map[string]any{"repo": "o/r"})
	e.mock.ExpectQuery("SELECT .+ FROM pipeline_connections WHERE id").WithArgs("p1").
		WillReturnRows(sqlmock.NewRows(apiPipelineCols).
			AddRow("p1", "ci", "github", cfgJSON, nil, "2026-06-10", "2026-06-10", nil))
	e.mock.ExpectQuery(`INSERT INTO health_runs[\s\S]*organization_id`).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), testActingOrg).
		WillReturnRows(healthRow("tok-1"))

	e.do(http.MethodPost, "/api/v1/health-lab/runs", `{"pipeline_connection_id":"p1"}`)
	if err := e.mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("the run was not stamped with the CALLER's organization: %v", err)
	}
}

// ---------------------------------------------------------------------------
// state_transfers

var transferFixtureCols = []string{"id", "mode", "source_id", "source_key", "target_source_id",
	"target_key", "status", "verified", "decommissioned", "detail", "actor", "created_at"}

func transferFixtureRow() *sqlmock.Rows {
	return sqlmock.NewRows(transferFixtureCols).
		AddRow("t1", "backup", "s1", "app.tfstate", "s2", "copy.tfstate", "success", true, false, "", "", "2026-06-10")
}

func TestTransfer_IsStampedWithTheActingOrganization(t *testing.T) {
	e := newSourcesEnv(t)
	dirB := t.TempDir()
	e.seed(t, "app.tfstate", minState(7, "lin-1", "aws_instance.web"))

	e.expectSource("s1", e.dir)
	e.expectSource("s2", dirB)
	e.mock.ExpectQuery(`INSERT INTO state_transfers[\s\S]*organization_id`).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), testActingOrg).
		WillReturnRows(transferFixtureRow())

	w := e.do(http.MethodPost, "/api/v1/sources/s1/state/backup?key=app.tfstate",
		`{"target_source_id":"s2","target_key":"copy.tfstate"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("backup: status = %d (%s)", w.Code, w.Body.String())
	}
	if err := e.mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("the INSERT did not name organization_id, or bound the wrong value: %v", err)
	}
}

// TestTransfer_AcrossOrganizationsIsAllowedWhenTheCallerHoldsBoth is the
// capability 000033 deliberately keeps. A transfer is how a state file crosses
// the boundary #436 draws, so this must NOT be refused just because the two ends
// disagree -- what makes it safe is that the caller holds authority on both
// sides, and that the row records whose act it was.
func TestTransfer_AcrossOrganizationsIsAllowedWhenTheCallerHoldsBoth(t *testing.T) {
	e := newSourcesEnvWithScope(t, tenantscope.Scope{OrgIDs: []string{testActingOrg, otherOrg}})
	dirB := t.TempDir()
	e.seed(t, "app.tfstate", minState(7, "lin-1", "aws_instance.web"))

	e.expectSource("s1", e.dir)                 // owned by testActingOrg
	e.expectSourceOwnedBy("s2", dirB, otherOrg) // owned by the other one

	// The caller belongs to BOTH organizations, so which one they are acting as is
	// genuinely ambiguous and the handler refuses until they say. That refusal is
	// the organization picker's whole job -- naming the acting organization is what
	// makes the audit record meaningful, since "the caller's organization" would
	// otherwise be a coin flip between the two.
	if w := e.do(http.MethodPost, "/api/v1/sources/s1/state/backup?key=app.tfstate",
		`{"target_source_id":"s2","target_key":"copy.tfstate"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("a caller in two organizations got status %d without naming one; want 400 (%s)",
			w.Code, w.Body.String())
	}

	// Named, it proceeds. Both source lookups have to be re-expected: the refused
	// attempt above consumed the first pair.
	e.expectSource("s1", e.dir)
	e.expectSourceOwnedBy("s2", dirB, otherOrg)
	e.mock.ExpectQuery(`INSERT INTO state_transfers[\s\S]*organization_id`).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), otherOrg).
		WillReturnRows(transferFixtureRow())
	// Acting as otherOrg while the REQUEST source belongs to testActingOrg. The
	// two values are deliberately different: everywhere they coincide, a handler
	// that stamped srcA.OrganizationID would be indistinguishable from one that
	// stamped the caller's, and the record's whole purpose is to name who
	// performed the act rather than which end it started from.
	w := e.doWithHeader(http.MethodPost, "/api/v1/sources/s1/state/backup?key=app.tfstate",
		`{"target_source_id":"s2","target_key":"copy.tfstate"}`,
		idtenantscope.ActingOrganizationHeader, otherOrg)
	if w.Code != http.StatusOK {
		t.Fatalf("cross-organization transfer was refused: status = %d (%s) -- this is a "+
			"SUPPORTED capability when the caller holds both ends", w.Code, w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(dirB, "copy.tfstate")); err != nil {
		t.Errorf("the transfer did not write the target: %v", err)
	}
}

// TestTransfer_RefusesAnEndTheCallerDoesNotHold is the other half: holding one
// end is not authority over the other one's state file.
func TestTransfer_RefusesAnEndTheCallerDoesNotHold(t *testing.T) {
	e := newSourcesEnv(t) // scope is testActingOrg only
	dirB := t.TempDir()
	e.seed(t, "app.tfstate", minState(7, "lin-1", "aws_instance.web"))

	e.expectSource("s1", e.dir)
	e.expectSourceOwnedBy("s2", dirB, otherOrg)
	// No INSERT expected, and no bytes written to the target.

	w := e.do(http.MethodPost, "/api/v1/sources/s1/state/backup?key=app.tfstate",
		`{"target_source_id":"s2","target_key":"copy.tfstate"}`)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (%s)", w.Code, w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(dirB, "copy.tfstate")); err == nil {
		t.Error("the transfer wrote to a target the caller may not reach")
	}
	if err := e.mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("a statement ran that should not have: %v", err)
	}
}

// ---------------------------------------------------------------------------
// helpers

// rowsOwnedBy rebuilds a pipeline fixture with a different owner in the last
// column. Rebuilt rather than mutated because sqlmock.Rows exposes no accessor.
func rowsOwnedBy(t *testing.T, _ *sqlmock.Rows, orgID string) *sqlmock.Rows {
	t.Helper()
	cfgJSON, _ := json.Marshal(map[string]any{"repo": "o/r"})
	return sqlmock.NewRows(apiPipelineCols).
		AddRow("p1", "ci", "github", cfgJSON, nil, "2026-06-10", "2026-06-10", orgID)
}

func (e *sourcesEnv) expectSourceOwnedBy(id, dir, orgID string) {
	cfg, _ := json.Marshal(map[string]any{"base_path": dir})
	e.mock.ExpectQuery("SELECT .+ FROM state_sources WHERE id").WithArgs(id).
		WillReturnRows(sqlmock.NewRows(apiSourceCols).
			AddRow(id, "local-"+id, "local", "", cfg, []byte(`{}`), nil, "2026-06-10", "2026-06-10", orgID))
}

var _ = gin.New
