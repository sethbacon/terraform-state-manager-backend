package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// A dispatch must not reach a pipeline connection, or aim at a state source,
// that the acting organization does not own.
//
// The credential is the reason this matters more than a read leak.
// resolvePipelineToken opens the connection's stored token — or the shared token
// of its CI source — and hands it to a real GitHub Actions or Azure DevOps
// dispatch. A check placed after that call is a check that runs with another
// tenant's secret already in memory and their pipeline already triggered.
//
// The rule lived at ONE of two sites: health.CreateRun compared the
// organizations and refused; dispatchDrift, the same operation for drift, did
// not. Both now go through pipelineConnectionFor.

func pipelineRowOwnedBy(t *testing.T, provider, token, orgID string) *sqlmock.Rows {
	t.Helper()
	cfgJSON, _ := json.Marshal(map[string]any{"owner": "o", "repo": "r"})
	return sqlmock.NewRows(apiPipelineCols).
		AddRow("p1", "ci", provider, cfgJSON, []byte(token), "2026-06-10", "2026-06-10", orgID)
}

// TestDriftDispatch_RefusesAConnectionOwnedElsewhere is the gap this closes.
func TestDriftDispatch_RefusesAConnectionOwnedElsewhere(t *testing.T) {
	e := newDriftEnv(t)
	e.mock.ExpectQuery("SELECT .+ FROM pipeline_connections WHERE id").WithArgs("p1").
		WillReturnRows(pipelineRowOwnedBy(t, "github_actions", "ghp_secret", otherTenant))
	// Nothing else is scripted: the refusal must happen before the CI-source
	// lookup that resolvePipelineToken would make, and before any INSERT.

	w := e.do(http.MethodPost, "/api/v1/drift/runs", `{"pipeline_connection_id":"p1"}`)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (%s)", w.Code, w.Body.String())
	}
	if err := e.mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("a statement ran past the ownership refusal: %v", err)
	}
}

// TestDriftDispatch_RefusesATargetSourceOwnedElsewhere covers the other half of
// a drift target: the connection may be yours while the source it aims at is not.
func TestDriftDispatch_RefusesATargetSourceOwnedElsewhere(t *testing.T) {
	e := newDriftEnv(t)
	e.mock.ExpectQuery("SELECT .+ FROM pipeline_connections WHERE id").WithArgs("p1").
		WillReturnRows(pipelineRowOwnedBy(t, "github_actions", "ghp_secret", testActingOrg))
	e.mock.ExpectQuery("SELECT .+ FROM state_sources WHERE id").WithArgs("s-other").
		WillReturnRows(sqlmock.NewRows(apiSourceCols).
			AddRow("s-other", "theirs", "local", "", []byte(`{}`), []byte(`{}`), nil,
				"2026-06-10", "2026-06-10", otherTenant))

	w := e.do(http.MethodPost, "/api/v1/drift/runs",
		`{"pipeline_connection_id":"p1","source_id":"s-other","state_key":"k"}`)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (%s)", w.Code, w.Body.String())
	}
	if err := e.mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("a statement ran past the ownership refusal: %v", err)
	}
}

// TestDriftDispatch_AllowsAnUnstampedConnection. A connection whose
// organization_id is still NULL predates the backfill; refusing it would break
// dispatch on exactly the rows #436 is repairing, and it makes no claim about
// ownership either way.
func TestDriftDispatch_AllowsAnUnstampedConnection(t *testing.T) {
	e := newDriftEnv(t)
	cfgJSON, _ := json.Marshal(map[string]any{"owner": "o"})
	e.mock.ExpectQuery("SELECT .+ FROM pipeline_connections WHERE id").WithArgs("p1").
		WillReturnRows(sqlmock.NewRows(apiPipelineCols).
			AddRow("p1", "ci", "github_actions", cfgJSON, []byte("ghp_x"), "2026-06-10", "2026-06-10", nil))
	e.mock.ExpectQuery("INSERT INTO drift_runs").WillReturnRows(driftRow("tok"))
	e.mock.ExpectQuery("UPDATE drift_runs").WillReturnRows(driftRow("tok"))

	// It gets past ownership; the incomplete GitHub config then fails the
	// dispatch, which is a 502 and not a 404 — the distinction under test.
	if w := e.do(http.MethodPost, "/api/v1/drift/runs", `{"pipeline_connection_id":"p1"}`); w.Code == http.StatusNotFound {
		t.Fatalf("an unstamped connection was refused as not-found (%s)", w.Body.String())
	}
}

// TestDriftDispatch_RefusesBeforeResolvingTheCredential pins the ORDER, which is
// the whole reason this check sits where it does.
//
// The connection below carries no token of its own, so resolvePipelineToken must
// go to ci_sources for its CI source's shared one. That lookup is scripted here
// precisely so it can go UNCONSUMED: sqlmock reports unfulfilled expectations,
// not extra calls, so an expectation that must NOT be met is the detectable
// direction. If the ownership check ever moves after the credential resolution,
// this query runs and the assertion flips.
func TestDriftDispatch_RefusesBeforeResolvingTheCredential(t *testing.T) {
	e := newDriftEnv(t)
	cfgJSON, _ := json.Marshal(map[string]any{"owner": "o", "ci_source_id": "c1"})
	e.mock.ExpectQuery("SELECT .+ FROM pipeline_connections WHERE id").WithArgs("p1").
		WillReturnRows(sqlmock.NewRows(apiPipelineCols).
			// nil token: the shared CI-source token is the only way to dispatch.
			AddRow("p1", "ci", "github_actions", cfgJSON, nil, "2026-06-10", "2026-06-10", otherTenant))
	e.mock.ExpectQuery("FROM ci_sources").WillReturnRows(sqlmock.NewRows(ciSrcCols))

	if w := e.do(http.MethodPost, "/api/v1/drift/runs", `{"pipeline_connection_id":"p1"}`); w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (%s)", w.Code, w.Body.String())
	}
	if err := e.mock.ExpectationsWereMet(); err == nil {
		t.Fatal("the CI-source lookup ran: the ownership check is happening AFTER the " +
			"credential is resolved, so another tenant's shared token was opened before " +
			"anything asked whether the caller was entitled to it")
	}
}
