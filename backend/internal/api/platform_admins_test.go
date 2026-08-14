package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"

	"github.com/terraform-state-manager/terraform-state-manager/internal/platformadmin"
)

// The management surface exists so a deployment can populate and correct its own
// carrier without hand-written SQL — which is also the one caller that can forget
// the audit intent. What these tests hold is the part a handler can get wrong:
// turning each of the module's DISTINCT refusals into a distinct answer.
//
// The statuses are not cosmetic. "There is genuinely nobody else" (409) is a
// conflict an operator resolves by granting somebody first; "I could not find out
// whether there is anybody else" (503) is an outage during which the last real
// administrator must not be allowed to remove themselves. A handler that served
// both as 409 — or worse, as 500 — would make the second indistinguishable from
// the first at exactly the moment the difference matters.

var paGrantCols = []string{"user_id", "granted_by", "granted_at", "note"}
var paUserCols = []string{"id", "email", "name", "oidc_sub", "created_at", "updated_at"}

const paUserID = "33333333-3333-4333-8333-333333333333"
const paOtherID = "44444444-4444-4444-8444-444444444444"

type paRig struct {
	router   *gin.Engine
	app      sqlmock.Sqlmock
	identity sqlmock.Sqlmock
}

// newPARig mounts the three routes over a carrier backed by two sqlmock handles,
// with a stub auth layer that publishes an acting principal (the handlers read it
// for the audit record's actor).
func newPARig(t *testing.T, wired bool) paRig {
	t.Helper()
	gin.SetMode(gin.TestMode)

	var svc *platformadmin.Service
	var appMock, identityMock sqlmock.Sqlmock
	if wired {
		appDB, am, err := sqlmock.New()
		if err != nil {
			t.Fatalf("sqlmock.New (app): %v", err)
		}
		t.Cleanup(func() { appDB.Close() })
		identityDB, im, err := sqlmock.New()
		if err != nil {
			t.Fatalf("sqlmock.New (identity): %v", err)
		}
		t.Cleanup(func() { identityDB.Close() })
		appMock, identityMock = am, im
		svc, err = platformadmin.New(appDB, identityDB)
		if err != nil {
			t.Fatalf("platformadmin.New: %v", err)
		}
	}

	h := NewPlatformAdminHandlers(svc)
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set("user_id", "actor-1"); c.Next() })
	r.GET("/platform-admins", h.ListPlatformAdmins())
	r.POST("/platform-admins", h.GrantPlatformAdmin())
	r.DELETE("/platform-admins/:user_id", h.RevokePlatformAdmin())
	return paRig{router: r, app: appMock, identity: identityMock}
}

func (r paRig) do(method, path, body string) *httptest.ResponseRecorder {
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.router.ServeHTTP(w, req)
	return w
}

func (r paRig) expectUserResolves(exists bool) {
	q := r.identity.ExpectQuery("SELECT id, email, name, oidc_sub, created_at, updated_at")
	if !exists {
		q.WillReturnRows(sqlmock.NewRows(paUserCols))
		return
	}
	now := time.Now()
	q.WillReturnRows(sqlmock.NewRows(paUserCols).AddRow(paUserID, "a@b.c", "A", "sub", now, now))
}

// TestPlatformAdminRoutesReportAnUnwiredCarrier: 503, not 404. A route that is
// mounted but has no carrier behind it is a deployment problem an operator can
// fix; a 404 reads as "this build has no platform admins" and sends them looking
// for the wrong thing.
func TestPlatformAdminRoutesReportAnUnwiredCarrier(t *testing.T) {
	r := newPARig(t, false)
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodGet, "/platform-admins", ""},
		{http.MethodPost, "/platform-admins", `{"user_id":"` + paUserID + `"}`},
		{http.MethodDelete, "/platform-admins/" + paUserID, ""},
	} {
		if w := r.do(tc.method, tc.path, tc.body); w.Code != http.StatusServiceUnavailable {
			t.Errorf("%s %s: status = %d, want 503 (%s)", tc.method, tc.path, w.Code, w.Body.String())
		}
	}
}

func TestGrantPlatformAdminRejectsAMalformedID(t *testing.T) {
	r := newPARig(t, true)
	// No database expectations: a malformed id must be refused before anything
	// is asked of either connection, because reaching the resolver with it would
	// produce a driver error that reads as an identity outage (503).
	w := r.do(http.MethodPost, "/platform-admins", `{"user_id":"not-a-uuid"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (%s)", w.Code, w.Body.String())
	}
	if err := r.identity.ExpectationsWereMet(); err != nil {
		t.Errorf("a malformed id reached the identity store: %v", err)
	}
}

func TestGrantPlatformAdminRejectsAnUnknownUser(t *testing.T) {
	r := newPARig(t, true)
	r.expectUserResolves(false)

	w := r.do(http.MethodPost, "/platform-admins", `{"user_id":"`+paUserID+`"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (%s)", w.Code, w.Body.String())
	}
	if err := r.app.ExpectationsWereMet(); err != nil {
		t.Errorf("the carrier was written for a user that does not exist: %v", err)
	}
}

// A re-grant is a CONFLICT, not a silent success: "already an admin" and
// "granted just now, by you" are different facts about who is accountable for
// the privilege, and the module preserves the original provenance rather than
// overwriting it.
func TestGrantPlatformAdminReportsAnExistingGrantAsAConflict(t *testing.T) {
	r := newPARig(t, true)
	r.expectUserResolves(true)
	r.app.ExpectBegin()
	// ON CONFLICT (user_id) DO NOTHING returns no row.
	r.app.ExpectQuery(`INSERT INTO "platform_admins"`).WillReturnError(errNoRowsForTest())
	r.app.ExpectRollback()

	w := r.do(http.MethodPost, "/platform-admins", `{"user_id":"`+paUserID+`"}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (%s)", w.Code, w.Body.String())
	}
}

func TestGrantPlatformAdminSucceeds(t *testing.T) {
	r := newPARig(t, true)
	r.expectUserResolves(true)
	r.app.ExpectBegin()
	r.app.ExpectQuery(`INSERT INTO "platform_admins"`).
		WillReturnRows(sqlmock.NewRows(paGrantCols).AddRow(paUserID, "actor-1", time.Now(), "on call"))
	r.app.ExpectExec(`INSERT INTO "audit_outbox"`).WillReturnResult(sqlmock.NewResult(0, 1))
	r.app.ExpectCommit()

	w := r.do(http.MethodPost, "/platform-admins", `{"user_id":"`+paUserID+`","note":"on call"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (%s)", w.Code, w.Body.String())
	}
	var got map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["user_id"] != paUserID {
		t.Errorf("user_id = %v, want %s", got["user_id"], paUserID)
	}
	if got["granted_by"] != "actor-1" {
		t.Errorf("granted_by = %v, want actor-1: the provenance is what this table is for", got["granted_by"])
	}
	if err := r.app.ExpectationsWereMet(); err != nil {
		t.Errorf("grant did not write its audit intent in its own transaction: %v", err)
	}
}

// TestListPlatformAdminsFlagsOrphans: the listing labels a grant whose user is
// gone rather than dropping it, because this is the only surface that can remove
// it.
func TestListPlatformAdminsFlagsOrphans(t *testing.T) {
	r := newPARig(t, true)
	r.app.ExpectQuery(`FROM "platform_admins"`).
		WillReturnRows(sqlmock.NewRows(paGrantCols).
			AddRow(paUserID, nil, time.Now(), "live").
			AddRow(paOtherID, nil, time.Now(), "deleted"))
	r.expectUserResolves(true)
	r.expectUserResolves(false)

	w := r.do(http.MethodGet, "/platform-admins", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	var got struct {
		PlatformAdmins []struct {
			UserID   string `json:"user_id"`
			Orphaned bool   `json:"orphaned"`
		} `json:"platform_admins"`
		Total int `json:"total"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Total != 2 {
		t.Fatalf("total = %d, want 2: an orphan must be listed", got.Total)
	}
	if got.PlatformAdmins[0].Orphaned {
		t.Errorf("%s flagged orphaned but its user resolves", got.PlatformAdmins[0].UserID)
	}
	if !got.PlatformAdmins[1].Orphaned {
		t.Errorf("%s not flagged orphaned but its user is gone", got.PlatformAdmins[1].UserID)
	}
}

// expectSerializedRevoke scripts everything up to and including the predicate's
// resolution: the advisory-lock transaction, the revoking transaction, and the
// FOR UPDATE read that returns rows.
func (r paRig) expectSerializedRevoke(remaining ...string) {
	r.app.ExpectBegin() // Serialize's lock-only transaction
	r.app.ExpectExec("pg_advisory_xact_lock").WillReturnResult(sqlmock.NewResult(0, 0))
	r.app.ExpectBegin() // Revoke's own transaction
	rows := sqlmock.NewRows(paGrantCols).AddRow(paUserID, nil, time.Now(), nil)
	for _, id := range remaining {
		rows = rows.AddRow(id, nil, time.Now(), nil)
	}
	r.app.ExpectQuery(`FROM "platform_admins" ORDER BY granted_at ASC, user_id ASC FOR UPDATE`).
		WillReturnRows(rows)
}

// TestRevokePlatformAdminRefusesTheLastOne. 409 with a remedy in the message:
// the operator's next action is to grant somebody, not to retry.
func TestRevokePlatformAdminRefusesTheLastOne(t *testing.T) {
	r := newPARig(t, true)
	r.expectSerializedRevoke() // the target is the only grant
	r.app.ExpectRollback()
	r.app.ExpectRollback()

	w := r.do(http.MethodDelete, "/platform-admins/"+paUserID, "")
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (%s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "grant another one first") {
		t.Errorf("body = %s, want the remedy named", w.Body.String())
	}
}

// TestRevokePlatformAdminRefusesWhenTheRemainingGrantIsAnOrphan. The row count
// says two; the exercisable count says one. Counting rows here is the defect the
// floor predicate exists to prevent — the deployment would be left with a table
// full of administrators and nobody who can log in as one.
func TestRevokePlatformAdminRefusesWhenTheRemainingGrantIsAnOrphan(t *testing.T) {
	r := newPARig(t, true)
	r.expectSerializedRevoke(paOtherID)
	r.expectUserResolves(false) // the remaining grant names a deleted user
	r.app.ExpectRollback()
	r.app.ExpectRollback()

	w := r.do(http.MethodDelete, "/platform-admins/"+paUserID, "")
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (%s)", w.Code, w.Body.String())
	}
}

// TestRevokePlatformAdminReportsAnIdentityOutageAsRetryable. 503, and NOT 409:
// nothing was changed and the answer is unresolved rather than negative.
func TestRevokePlatformAdminReportsAnIdentityOutageAsRetryable(t *testing.T) {
	r := newPARig(t, true)
	r.expectSerializedRevoke(paOtherID)
	r.identity.ExpectQuery("SELECT id, email, name, oidc_sub, created_at, updated_at").
		WillReturnError(errors.New("dial tcp: connection refused"))
	r.app.ExpectRollback()
	r.app.ExpectRollback()

	w := r.do(http.MethodDelete, "/platform-admins/"+paUserID, "")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (%s)", w.Code, w.Body.String())
	}
}

func TestRevokePlatformAdminReportsAMissingGrantAsNotFound(t *testing.T) {
	r := newPARig(t, true)
	r.app.ExpectBegin()
	r.app.ExpectExec("pg_advisory_xact_lock").WillReturnResult(sqlmock.NewResult(0, 0))
	r.app.ExpectBegin()
	// The carrier holds somebody else entirely.
	r.app.ExpectQuery(`FROM "platform_admins" ORDER BY granted_at ASC, user_id ASC FOR UPDATE`).
		WillReturnRows(sqlmock.NewRows(paGrantCols).AddRow(paOtherID, nil, time.Now(), nil))
	r.app.ExpectRollback()
	r.app.ExpectRollback()

	w := r.do(http.MethodDelete, "/platform-admins/"+paUserID, "")
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (%s)", w.Code, w.Body.String())
	}
}

func TestRevokePlatformAdminSucceedsWhenAnotherAdminRemains(t *testing.T) {
	r := newPARig(t, true)
	r.expectSerializedRevoke(paOtherID)
	r.expectUserResolves(true) // the remaining grant is exercisable
	r.app.ExpectExec(`DELETE FROM "platform_admins"`).WillReturnResult(sqlmock.NewResult(0, 1))
	r.app.ExpectExec(`INSERT INTO "audit_outbox"`).WillReturnResult(sqlmock.NewResult(0, 1))
	r.app.ExpectCommit()
	r.app.ExpectRollback() // Serialize's lock transaction

	w := r.do(http.MethodDelete, "/platform-admins/"+paUserID, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	if err := r.app.ExpectationsWereMet(); err != nil {
		t.Errorf("the revocation did not write its intent in the delete's own transaction: %v", err)
	}
}

// errNoRowsForTest is sql.ErrNoRows, which is what ON CONFLICT (user_id) DO
// NOTHING produces when the row already exists and the RETURNING clause matches
// nothing. Named rather than inlined so the reason is stated once.
func errNoRowsForTest() error { return sql.ErrNoRows }
