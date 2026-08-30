package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/terraform-state-manager/terraform-state-manager/internal/tenancy"
)

// A dispatch must not reach a pipeline connection, aim at a state source, or
// open a CI source that the dispatching organization does not own.
//
// The credential is the reason this matters more than a read leak.
// resolvePipelineToken opens the connection's stored token — or the shared token
// of its CI source — and hands it to a real GitHub Actions or Azure DevOps
// dispatch. A check placed after that call is a check that runs with another
// tenant's secret already in memory and their pipeline already triggered.
//
// Since #393's option B the rule is not a comparison but a SCOPE: every by-id
// load on the chain is an InScope read under one single-organization authority
// (request-resolved here; system-derived on the scheduler path), so a row in
// another organization matches no row at all. These tests pin that the
// statements sent to the database CARRY the organization predicate — the
// refusal happens in SQL, not in Go code that a new call site could forget.

func pipelineRowOwnedBy(t *testing.T, provider, token, orgID string) *sqlmock.Rows {
	t.Helper()
	cfgJSON, _ := json.Marshal(map[string]any{"owner": "o", "repo": "r"})
	return sqlmock.NewRows(apiPipelineCols).
		AddRow("p1", "ci", provider, cfgJSON, []byte(token), "2026-06-10", "2026-06-10", orgID)
}

// TestDriftDispatch_RefusesAConnectionOwnedElsewhere is the gap this closes.
//
// The scoped read binds the acting organization as $1 and the id as $2, so a
// connection owned elsewhere matches nothing. The empty result — not a Go-side
// comparison — is the refusal, and it must surface as the same 404 an absent id
// gets.
func TestDriftDispatch_RefusesAConnectionOwnedElsewhere(t *testing.T) {
	e := newDriftEnv(t)
	e.mock.ExpectQuery("FROM pipeline_connections WHERE organization_id = ANY").
		WithArgs([]string{testActingOrg}, "p1").
		WillReturnRows(sqlmock.NewRows(apiPipelineCols)) // owned by another org: no row matches
	// Nothing else is scripted: the refusal must happen before the CI-source
	// lookup that resolvePipelineToken would make, and before any INSERT.

	w := e.do(http.MethodPost, "/api/v1/drift/runs", `{"pipeline_connection_id":"p1"}`)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (%s)", w.Code, w.Body.String())
	}
	if err := e.mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("a statement ran past the scoped-load refusal: %v", err)
	}
}

// TestDriftDispatch_RefusesATargetSourceOwnedElsewhere covers the other half of
// a drift target: the connection may be yours while the source it aims at is not.
func TestDriftDispatch_RefusesATargetSourceOwnedElsewhere(t *testing.T) {
	e := newDriftEnv(t)
	e.mock.ExpectQuery("FROM pipeline_connections WHERE organization_id = ANY").
		WithArgs([]string{testActingOrg}, "p1").
		WillReturnRows(pipelineRowOwnedBy(t, "github_actions", "ghp_secret", testActingOrg))
	e.mock.ExpectQuery("FROM state_sources WHERE organization_id = ANY").
		WithArgs([]string{testActingOrg}, "s-other").
		WillReturnRows(sqlmock.NewRows(apiSourceCols)) // another organization's source: no row

	w := e.do(http.MethodPost, "/api/v1/drift/runs",
		`{"pipeline_connection_id":"p1","source_id":"s-other","state_key":"k"}`)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (%s)", w.Code, w.Body.String())
	}
	if err := e.mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("a statement ran past the scoped-load refusal: %v", err)
	}
}

// TestDriftDispatch_RefusesAnUnstampedConnection REVERSES this file's previous
// pin, deliberately, and the history matters.
//
// While the backfill was still repairing pre-#436 rows, an unstamped connection
// was allowed through: refusing it would have broken dispatch on exactly the
// rows being repaired. Migration 000034 then made organization_id NOT NULL on
// all nine roots, so the schema can no longer produce such a row — only a
// database restored from a pre-000034 backup can — and the #393 option-B
// decision settles what a scoped chain does with one: a row that belongs to no
// organization is dispatched by NO ONE. The organization predicate excludes
// NULL, the row matches nothing, and dispatch answers 404 until the boot
// backfill stamps it. Fail-closed, same as every scoped reader.
func TestDriftDispatch_RefusesAnUnstampedConnection(t *testing.T) {
	e := newDriftEnv(t)
	// The scoped statement runs; a NULL organization_id cannot match
	// `organization_id = ANY(...)`, so the fixture returns no row.
	e.mock.ExpectQuery("FROM pipeline_connections WHERE organization_id = ANY").
		WithArgs([]string{testActingOrg}, "p1").
		WillReturnRows(sqlmock.NewRows(apiPipelineCols))

	w := e.do(http.MethodPost, "/api/v1/drift/runs", `{"pipeline_connection_id":"p1"}`)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (%s). An unstamped connection belongs to no tenant; "+
			"dispatching it would run CI under an authority nothing derived.", w.Code, w.Body.String())
	}
	if err := e.mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("a statement ran past the scoped-load refusal: %v", err)
	}
}

// TestDriftDispatch_ScopesTheCISourceHop pins the load that used to be entirely
// unscoped — the confused-deputy hop the #393 decision names by name.
//
// The connection is OURS but carries no token of its own, so
// resolvePipelineToken must go to ci_sources for the shared credential. That
// read now binds the SAME single-organization authority as every other load on
// the chain ($1 = the acting organization's array), so a config pointing at
// another organization's CI source matches no row and the dispatch fails —
// BEFORE any drift run is inserted and before any provider call.
func TestDriftDispatch_ScopesTheCISourceHop(t *testing.T) {
	e := newDriftEnv(t)
	cfgJSON, _ := json.Marshal(map[string]any{"owner": "o", "ci_source_id": "c-theirs"})
	e.mock.ExpectQuery("FROM pipeline_connections WHERE organization_id = ANY").
		WithArgs([]string{testActingOrg}, "p1").
		WillReturnRows(sqlmock.NewRows(apiPipelineCols).
			// nil token: the shared CI-source token is the only way to dispatch.
			AddRow("p1", "ci", "github_actions", cfgJSON, nil, "2026-06-10", "2026-06-10", testActingOrg))
	e.mock.ExpectQuery("FROM ci_sources WHERE organization_id = ANY").
		WithArgs([]string{testActingOrg}, "c-theirs").
		WillReturnRows(sqlmock.NewRows(ciSrcCols)) // another organization's source: no row
	// Nothing further: no INSERT INTO drift_runs, no provider call.

	w := e.do(http.MethodPost, "/api/v1/drift/runs", `{"pipeline_connection_id":"p1"}`)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (%s). The connection is the caller's own; its poisoned "+
			"reference is a server-side fault, reported without confirming whether the id exists "+
			"elsewhere.", w.Code, w.Body.String())
	}
	if err := e.mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("a statement ran past the scoped CI-source load: %v", err)
	}
}

// TestDriftDispatch_RefusesBeforeResolvingTheCredential pins the ORDER, which is
// the whole reason the connection load comes first.
//
// The connection below is owned elsewhere, so the scoped load returns no row.
// The CI-source lookup is scripted here precisely so it can go UNCONSUMED:
// sqlmock reports unfulfilled expectations, not extra calls, so an expectation
// that must NOT be met is the detectable direction. If the refusal ever moves
// after the credential resolution, this query runs and the assertion flips.
func TestDriftDispatch_RefusesBeforeResolvingTheCredential(t *testing.T) {
	e := newDriftEnv(t)
	e.mock.ExpectQuery("FROM pipeline_connections WHERE organization_id = ANY").
		WithArgs([]string{testActingOrg}, "p1").
		WillReturnRows(sqlmock.NewRows(apiPipelineCols)) // owned elsewhere: no row
	e.mock.ExpectQuery("FROM ci_sources").WillReturnRows(sqlmock.NewRows(ciSrcCols))

	if w := e.do(http.MethodPost, "/api/v1/drift/runs", `{"pipeline_connection_id":"p1"}`); w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (%s)", w.Code, w.Body.String())
	}
	if err := e.mock.ExpectationsWereMet(); err == nil {
		t.Fatal("the CI-source lookup ran: the refusal is happening AFTER the " +
			"credential is resolved, so another tenant's shared token was opened before " +
			"anything asked whether the dispatch was entitled to it")
	}
}

// TestDispatchAuthorityNeverConfersMoreThanOneOrganization pins the DERIVATION,
// which the sqlmock cases above structurally cannot.
//
// Those cases hand a mock the rows they expect back; the mock never evaluates a
// predicate, so a scoped reader and an unscoped one are indistinguishable to
// them. Proved by mutation during review: replacing the derived scope with
// `tenantscope.Scope{PlatformAdmin: true}` -- which makes every InScope reader
// take its bypass branch and serve any organization's row -- compiles and
// leaves the whole dispatch suite, and even a live-PostgreSQL test of the
// repository predicate, green. The predicate was never the weak point; the
// authority handed to it was.
//
// So this asserts the two properties of the derivation itself: it confers
// exactly the one organization it was derived from, and it never confers the
// platform-admin bypass. A dispatch that could take that branch is the
// confused deputy the whole design exists to close.
func TestDispatchAuthorityNeverConfersMoreThanOneOrganization(t *testing.T) {
	const org = "11111111-1111-4111-8111-111111111111"

	for _, c := range []struct {
		name string
		auth dispatchAuthority
	}{
		{"request-resolved", requestAuthority(org)},
		{"system-derived", systemAuthority(mustSystemScope(t, org))},
	} {
		t.Run(c.name, func(t *testing.T) {
			sc := organizationScope(c.auth.organizationID)
			if sc.PlatformAdmin {
				t.Fatal("the dispatch authority confers the platform-admin bypass: every InScope " +
					"reader under it serves any organization's row, so a chain crossing " +
					"organizations no longer fails closed")
			}
			if len(sc.OrgIDs) != 1 || sc.OrgIDs[0] != org {
				t.Fatalf("the dispatch authority confers %v, want exactly [%s]: authority must be "+
					"the one organization it was derived from", sc.OrgIDs, org)
			}
		})
	}

	// An authority derived from nothing must permit nothing, or an unstamped
	// row would dispatch under a scope that reads everything.
	if sc := organizationScope(""); !sc.Empty() || sc.PlatformAdmin {
		t.Fatalf("an authority derived from no organization permits %v (platformAdmin=%v), want nothing",
			sc.OrgIDs, sc.PlatformAdmin)
	}
}

// mustSystemScope builds the system authority the scheduler derives, failing
// the test rather than returning a zero value that would silently permit
// nothing and make the assertions above pass for the wrong reason.
func mustSystemScope(t *testing.T, org string) tenancy.SystemScope {
	t.Helper()
	s, err := tenancy.SystemActingIn(org, "schedules", "00000000-0000-4000-8000-000000000001")
	if err != nil {
		t.Fatalf("SystemActingIn(%s): %v", org, err)
	}
	return s
}
