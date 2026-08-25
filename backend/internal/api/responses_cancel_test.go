package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// #487 — a request the CLIENT abandoned must not be reported as a server fault.
//
// The frontend fires /auth/me as the OIDC callback is still settling and then
// aborts it, so every login produced a 500 and an ERROR line for ordinary
// client behaviour, in the one stream where ERROR is meant to mean something is
// wrong.

func cancelledRequestContext(t *testing.T) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	ctx, cancel := context.WithCancel(context.Background())
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil).WithContext(ctx)
	cancel() // the client hung up
	return c, w
}

func TestServerError_ClientCancellationAnswers499AndRecordsNoError(t *testing.T) {
	c, w := cancelledRequestContext(t)

	// The shape the real failure arrives in: wrapped with %w, several layers
	// down, exactly as approles.rolesByKey produces it.
	err := fmt.Errorf("approles: reading roles for user %s: %w", "user-1", context.Canceled)
	serverError(c, err, "Failed to get user information")

	if w.Code != StatusClientClosedRequest {
		t.Errorf("status = %d, want %d (client closed request)", w.Code, StatusClientClosedRequest)
	}
	if w.Code >= 500 {
		t.Errorf("status %d is in the 5xx band, which is what alerting watches", w.Code)
	}
	// Nothing attached: the access log surfaces c.Errors to explain a 5xx, and
	// this is not one.
	if len(c.Errors) != 0 {
		t.Errorf("c.Errors = %v, want empty for an abandoned request", c.Errors)
	}
	if body := w.Body.String(); body != "" {
		t.Errorf("body = %q, want empty — nobody is listening", body)
	}
}

// The distinction that makes this safe: a context the SERVER cancelled yields
// the same error value and is a genuine fault. If this ever regresses to a bare
// errors.Is, real 500s start reporting as client disconnects and disappear from
// alerting.
func TestServerError_ServerSideCancellationKeepsIts500(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	// The REQUEST context is alive; only an internal sub-context was cancelled.
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)

	err := fmt.Errorf("internal timeout: %w", context.Canceled)
	serverError(c, err, "Failed to get user information")

	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500: a cancellation the server caused is still a server fault", w.Code)
	}
	if len(c.Errors) != 1 {
		t.Errorf("c.Errors = %v, want the cause attached for the access log", c.Errors)
	}
}

func TestServerError_OrdinaryErrorIsUnchanged(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/sources", nil)

	serverError(c, errors.New("database is on fire"), "Failed to list sources")

	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
	if len(c.Errors) != 1 {
		t.Errorf("c.Errors = %v, want the cause attached", c.Errors)
	}
	if body := w.Body.String(); body == "" {
		t.Error("an ordinary 500 must still carry its generic body")
	}
}
