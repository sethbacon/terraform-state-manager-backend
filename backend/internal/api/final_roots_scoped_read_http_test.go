package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
)

// The HTTP half of the last two Phase 3 read flips — notification_channels and
// state_transfers (#393).
//
// WHAT THESE CAN AND CANNOT SAY. sqlmock does not evaluate a predicate, so
// nothing here proves another organization's row is withheld; that is proved
// against a real PostgreSQL in internal/tenancy/final_roots_integration_test.go,
// including that the notification channel's SECRET TARGET does not cross. What
// these cover is the layer above the repository and the one a live-database test
// cannot reach: which STATEMENT the handler chooses, which organization it binds,
// and what it does when no scope was resolved at all.
//
// The unresolved-scope cases are the reason this file exists at all. An empty
// scope is an answer and reading with it returns nothing; an UNRESOLVED scope
// means the route lost its middleware.TenantScope, and the difference between
// 500 and "you have no channels" is the difference between noticing that and not.

// ---------------------------------------------------------------------------
// notification_channels
// ---------------------------------------------------------------------------

// TestListChannels_ReadsOnlyTheCallersOrganizations is the flip itself: the
// statement now carries the tenant predicate and binds the caller's
// organizations. Before this, it was `SELECT ... FROM notification_channels
// ORDER BY created_at DESC` for every caller in the deployment.
func TestListChannels_ReadsOnlyTheCallersOrganizations(t *testing.T) {
	e := newNotificationsEnv(t)

	e.mock.ExpectQuery("FROM notification_channels WHERE organization_id = ANY.+ORDER BY").
		WithArgs([]string{testActingOrg}).
		WillReturnRows(notifChannelRow(t, "https://hooks.example.com/x"))

	w := e.do(http.MethodGet, "/api/v1/notifications/channels", "")
	if w.Code != http.StatusOK {
		t.Fatalf("list: status = %d (%s)", w.Code, w.Body.String())
	}
	if err := e.mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("the list did not bind the caller's organizations: %v", err)
	}
	// The list has never returned the target and must not start: the assertion is
	// kept beside the scoping one because both are about the same secret.
	if strings.Contains(w.Body.String(), "hooks.example.com") {
		t.Error("the scoped list leaked a channel target")
	}
}

// TestChannelRoutes_UnresolvedScopeIsRefusedBeforeAnyStatement covers all four
// reads and mutations at once, because they share one resolver (channelScope)
// and a fault in it is a fault in all of them.
//
// EXACTLY ONE RESPONSE is asserted, not merely the right status: a handler that
// writes the refusal and then carries on still emits this body before failing
// further down, so the substring check alone passes either way. Two concatenated
// JSON objects do not unmarshal, and that is what proves the handler stopped.
func TestChannelRoutes_UnresolvedScopeIsRefusedBeforeAnyStatement(t *testing.T) {
	for _, tc := range []struct{ name, method, path, body string }{
		{"list", http.MethodGet, "/api/v1/notifications/channels", ""},
		{"update", http.MethodPut, "/api/v1/notifications/channels/n1", `{"name":"ops","type":"webhook"}`},
		{"delete", http.MethodDelete, "/api/v1/notifications/channels/n1", ""},
		{"test-send", http.MethodPost, "/api/v1/notifications/channels/n1/test", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newNotificationsEnvWithoutScope(t)
			// No expectations queued: reaching the database at all is the failure.
			w := e.do(tc.method, tc.path, tc.body)
			if w.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want 500 (%s)", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), "tenant scope was not resolved") {
				t.Fatalf("500 for the wrong reason: %s", w.Body.String())
			}
			var body map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatalf("the handler wrote more than one response (%s): the refusal did not "+
					"return, so the request continued past it", w.Body.String())
			}
			if err := e.mock.ExpectationsWereMet(); err != nil {
				t.Fatalf("a statement ran with no tenant scope resolved: %v", err)
			}
		})
	}
}

// TestTestChannel_AsksWhoOwnsItBeforeDecryptingItsTarget is the one that matters
// most on this root, and the ordering is the assertion.
//
// The shared library's SendTest takes no query option, so the ownership question
// has to be asked by this handler — and it has to be asked BEFORE SendTest,
// which loads the channel, decrypts its capability-bearing target and POSTs to
// it. Only the scoped read is queued here: if the handler asked afterwards, or
// not at all, the unscoped load would run against an empty expectation set and
// this would fail rather than pass.
func TestTestChannel_AsksWhoOwnsItBeforeDecryptingItsTarget(t *testing.T) {
	e := newNotificationsEnv(t)

	e.mock.ExpectQuery("FROM notification_channels WHERE id .+ organization_id = ANY").
		WithArgs("n1", []string{testActingOrg}).
		WillReturnRows(sqlmock.NewRows(notifChannelCols)) // no row: not this caller's

	w := e.do(http.MethodPost, "/api/v1/notifications/channels/n1/test", "")
	if w.Code != http.StatusNotFound {
		t.Fatalf("test-send on a channel the caller does not own: status = %d, want 404 (%s)",
			w.Code, w.Body.String())
	}
	if err := e.mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("the scoped ownership read did not run, or ran with the wrong organization: %v", err)
	}
}

// ---------------------------------------------------------------------------
// state_transfers
// ---------------------------------------------------------------------------

var transferAPICols = []string{"id", "mode", "source_id", "source_key", "target_source_id",
	"target_key", "status", "verified", "decommissioned", "detail", "actor", "created_at"}

// TestGetTransfer_ReadsInScope: the record names both source ids and both state
// keys, so an unscoped by-id read handed any caller holding state:read a map of
// another tenant's state files.
func TestGetTransfer_ReadsInScope(t *testing.T) {
	e := newSourcesEnv(t)

	e.mock.ExpectQuery("FROM state_transfers WHERE organization_id = ANY.+AND id").
		WithArgs([]string{testActingOrg}, "t1").
		WillReturnRows(sqlmock.NewRows(transferAPICols).
			AddRow("t1", "migrate", "s1", "prod.tfstate", "s2", "prod.tfstate",
				"success", true, false, "ok", "alice", "2026-08-30"))

	w := e.do(http.MethodGet, "/api/v1/transfers/t1", "")
	if w.Code != http.StatusOK {
		t.Fatalf("get transfer: status = %d (%s)", w.Code, w.Body.String())
	}
	if err := e.mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("the transfer read did not bind the caller's organizations: %v", err)
	}
}

// A transfer the caller's scope does not reach is answered EXACTLY as one that
// does not exist. Anything else lets a caller enumerate transfer ids and learn
// which of them name real moves elsewhere in the deployment.
func TestGetTransfer_OutsideTheScopeIsNotFound(t *testing.T) {
	e := newSourcesEnv(t)

	e.mock.ExpectQuery("FROM state_transfers WHERE organization_id = ANY.+AND id").
		WithArgs([]string{testActingOrg}, "someone-elses").
		WillReturnRows(sqlmock.NewRows(transferAPICols))

	w := e.do(http.MethodGet, "/api/v1/transfers/someone-elses", "")
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (%s)", w.Code, w.Body.String())
	}
	if strings.Contains(strings.ToLower(w.Body.String()), "organization") ||
		strings.Contains(strings.ToLower(w.Body.String()), "forbidden") {
		t.Errorf("the refusal disclosed that the transfer exists elsewhere: %s", w.Body.String())
	}
	if err := e.mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestGetTransfer_UnresolvedScopeIsRefusedBeforeTheRead(t *testing.T) {
	e := newSourcesEnvWithoutScope(t)
	// No expectations: reaching the database at all is the failure.
	w := e.do(http.MethodGet, "/api/v1/transfers/t1", "")
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (%s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "tenant scope was not resolved") {
		t.Fatalf("500 for the wrong reason: %s", w.Body.String())
	}
	if err := e.mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("a statement ran with no tenant scope resolved: %v", err)
	}
}
