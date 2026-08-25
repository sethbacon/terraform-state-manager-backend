package middleware

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// captureAccessLog runs one request through AccessLog and returns the decoded
// log record it emitted.
func captureAccessLog(t *testing.T, cancelClient bool, handler gin.HandlerFunc) map[string]any {
	t.Helper()
	gin.SetMode(gin.TestMode)

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	r := gin.New()
	r.Use(AccessLog())
	r.GET("/api/v1/auth/me", handler)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
	if cancelClient {
		ctx, cancel := context.WithCancel(req.Context())
		req = req.WithContext(ctx)
		cancel()
	}
	r.ServeHTTP(httptest.NewRecorder(), req)

	line := strings.TrimSpace(buf.String())
	if line == "" {
		t.Fatal("AccessLog emitted nothing")
	}
	var rec map[string]any
	if err := json.Unmarshal([]byte(line), &rec); err != nil {
		t.Fatalf("decode log line %q: %v", line, err)
	}
	return rec
}

// #487 — an ERROR line on every login makes the level meaningless for the
// faults it is supposed to surface.
//
// api.serverError answers 499 for the paths that go through it, but 33 handlers
// write a 500 directly. This layer cannot change the status those already sent;
// it must still keep the client disconnect out of the Error stream.
func TestAccessLog_CancelledRequestIsNotLoggedAsAnError(t *testing.T) {
	rec := captureAccessLog(t, true, func(c *gin.Context) {
		// A handler that bypasses serverError, as 33 of them do.
		c.JSON(http.StatusInternalServerError, gin.H{"error": "boom"})
	})

	if lvl, _ := rec["level"].(string); lvl == "ERROR" {
		t.Errorf("a request the client abandoned was logged at ERROR: %v", rec)
	}
	if v, ok := rec["client_disconnected"].(bool); !ok || !v {
		t.Errorf("client_disconnected marker missing, so the line cannot be told apart from an ordinary 200: %v", rec)
	}
}

// The falsification: a REAL 500 on a live connection must still be ERROR. A fix
// that silenced every 500 would pass the test above and destroy the signal.
func TestAccessLog_RealServerErrorIsStillLoggedAsAnError(t *testing.T) {
	rec := captureAccessLog(t, false, func(c *gin.Context) {
		_ = c.Error(context.DeadlineExceeded)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "boom"})
	})

	if lvl, _ := rec["level"].(string); lvl != "ERROR" {
		t.Errorf("level = %q, want ERROR for a genuine 500: %v", lvl, rec)
	}
	if _, present := rec["client_disconnected"]; present {
		t.Errorf("a live request must not be marked as a client disconnect: %v", rec)
	}
}

// An ordinary 200 keeps its Info line and gains no marker.
func TestAccessLog_SuccessfulRequestIsUnchanged(t *testing.T) {
	rec := captureAccessLog(t, false, func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})
	if lvl, _ := rec["level"].(string); lvl != "INFO" {
		t.Errorf("level = %q, want INFO", lvl)
	}
	if _, present := rec["client_disconnected"]; present {
		t.Errorf("unexpected client_disconnected marker: %v", rec)
	}
}
