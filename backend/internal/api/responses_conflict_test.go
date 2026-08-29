package api

// responses_conflict_test.go covers #486: a unique-constraint violation reported
// as a server fault.
//
// The reported route was POST /api/v1/admin/organizations/{id}/members, where
// adding a user who is already a member returned 500. The fix is in serverError
// rather than in that handler, because serverError is the single funnel for 147
// call sites and many of them are INSERTs into tables with unique constraints --
// so these tests are written against the funnel, which is where the class lives.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgconn"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
)

// theReportedError reproduces the error from the issue, verbatim.
func theReportedError() error {
	return &pgconn.PgError{
		Code:           "23505",
		Message:        `duplicate key value violates unique constraint "organization_members_organization_id_user_id_key"`,
		ConstraintName: "organization_members_organization_id_user_id_key",
	}
}

func runServerError(t *testing.T, err error, msg string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/admin/organizations/o1/members", nil)
	serverError(c, err, msg)
	return w
}

func TestUniqueViolationIsAConflictNotAServerError(t *testing.T) {
	w := runServerError(t, fmt.Errorf("failed to add member: %w", theReportedError()), "failed to add member")

	if w.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409. A client-side conflict reported as 5xx makes retry logic "+
			"treat 'already a member' as an outage.", w.Code)
	}
}

// TestConflictResponseDoesNotLeakSchemaDetail.
//
// NOTE ON THE ISSUE'S SECOND CLAIM: it says the constraint name and SQLSTATE are
// "echoed to the client in the error field". They are not -- serverError has
// always replied with the caller's generic msg, and the JSON quoted in the issue
// is the ACCESS LOG line, not the response body. This test pins the property the
// issue was reaching for, so it stays true whatever else changes.
func TestConflictResponseDoesNotLeakSchemaDetail(t *testing.T) {
	w := runServerError(t, fmt.Errorf("failed to add member: %w", theReportedError()), "failed to add member")

	body := w.Body.String()
	for _, leak := range []string{"organization_members", "23505", "duplicate key", "unique constraint", "SQLSTATE"} {
		if strings.Contains(body, leak) {
			t.Errorf("response body leaks %q: %s", leak, body)
		}
	}
	var got map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("body is not the usual {\"error\": ...} shape: %v", err)
	}
	if got["error"] != "failed to add member" {
		t.Errorf("error = %q, want the caller's message passed through unchanged", got["error"])
	}
}

// TestConflictStillReachesTheServerLog is the half that must not be lost.
//
// Downgrading the status is only correct if the cause is still diagnosable. The
// access log emits c.Errors keyed by request id, so the constraint violation
// must still be attached to the context -- 499 deliberately records nothing,
// and copying that would have thrown away the detail.
func TestConflictStillReachesTheServerLog(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/x", nil)

	serverError(c, fmt.Errorf("failed to add member: %w", theReportedError()), "failed to add member")

	if len(c.Errors) == 0 {
		t.Fatal("the violation was not recorded on the context, so it appears in no server-side log " +
			"and the operator has a 409 with no cause")
	}
	if !strings.Contains(c.Errors.String(), "23505") {
		t.Errorf("recorded error lost the SQLSTATE: %s", c.Errors.String())
	}
}

// TestConflictDropsOutOfTheErrorLogBand is the issue's third claim, checked
// rather than assumed.
//
// AccessLog picks slog.Error on status >= 500. The whole "noise that masks real
// faults" complaint is resolved by the status change alone -- but only if that
// threshold is really what gates the level, so this asserts the boundary
// instead of trusting a code reading.
func TestConflictDropsOutOfTheErrorLogBand(t *testing.T) {
	w := runServerError(t, fmt.Errorf("wrapped: %w", theReportedError()), "failed to add member")
	if w.Code >= 500 {
		t.Errorf("status %d is still in the band AccessLog logs at Error level", w.Code)
	}
	// And a genuine fault must stay in it, or the fix has traded one wrong
	// level for another.
	w2 := runServerError(t, errors.New("connection refused"), "failed to add member")
	if w2.Code < 500 {
		t.Errorf("a non-constraint error got status %d; real faults must stay 5xx", w2.Code)
	}
}

// TestOnlyUniqueViolationsBecomeConflicts guards the blast radius. This branch
// sits in front of 147 call sites, so anything it over-matches turns a genuine
// fault into a 409 that alerting ignores.
func TestOnlyUniqueViolationsBecomeConflicts(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{"unique violation", theReportedError(), http.StatusConflict},
		{"wrapped unique violation", fmt.Errorf("a: %w", fmt.Errorf("b: %w", theReportedError())), http.StatusConflict},
		{"foreign key violation", &pgconn.PgError{Code: "23503"}, http.StatusInternalServerError},
		{"not-null violation", &pgconn.PgError{Code: "23502"}, http.StatusInternalServerError},
		{"check violation", &pgconn.PgError{Code: "23514"}, http.StatusInternalServerError},
		{"serialization failure", &pgconn.PgError{Code: "40001"}, http.StatusInternalServerError},
		{"plain error", errors.New("boom"), http.StatusInternalServerError},
		{"nil error", nil, http.StatusInternalServerError},
		// The text alone must not be enough: a message that merely mentions a
		// duplicate is not a constraint violation.
		{"error text mentioning duplicate key", errors.New("duplicate key value violates unique constraint"), http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := runServerError(t, tc.err, "msg").Code; got != tc.want {
				t.Errorf("status = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestAddExistingOrganizationMemberIsAConflict is #486 on the route it was
// reported against, end to end through the real handler.
//
// The funnel tests above prove serverError translates the error. This proves
// the reported request actually reaches it -- that AddOrganizationMember has no
// earlier branch that swallows or re-wraps the violation into something
// isUniqueViolation cannot see, which is the way a funnel fix silently fails to
// reach the site that motivated it.
func TestAddExistingOrganizationMemberIsAConflict(t *testing.T) {
	e := newAdminWriteEnvWithApp(t)

	const roleID = "6e9a2b62-0e58-4b34-8f4b-2a6f9d3c1ab0"
	roleTemplateCols := []string{"id", "name", "display_name", "description", "scopes", "is_system", "created_at", "updated_at"}
	// The ceiling check and the write's own resolution both read the app row.
	for range 2 {
		e.mock.ExpectQuery("FROM role_templates WHERE").
			WithArgs(roleID).
			WillReturnRows(sqlmock.NewRows(roleTemplateCols).
				AddRow(roleID, "viewer", "Viewer", "read-only", []byte(`[]`), false, time.Now(), time.Now()))
	}
	e.mock.ExpectQuery("SELECT id FROM role_templates WHERE name").WithArgs("viewer").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(roleID))
	// The database answers exactly as the issue's log line shows.
	e.mock.ExpectExec("INSERT INTO organization_members").WillReturnError(theReportedError())

	w := e.do(http.MethodPost, "/api/v1/admin/organizations/o1/members",
		`{"user_id":"u1","role_template_id":"`+roleID+`"}`)

	if w.Code != http.StatusConflict {
		t.Fatalf("adding an existing member: status = %d, want 409 (body %s)", w.Code, w.Body.String())
	}
	for _, leak := range []string{"organization_members_organization_id_user_id_key", "23505", "SQLSTATE"} {
		if strings.Contains(w.Body.String(), leak) {
			t.Errorf("response leaks %q: %s", leak, w.Body.String())
		}
	}
}
