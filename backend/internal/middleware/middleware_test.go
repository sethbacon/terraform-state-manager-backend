package middleware

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/terraform-state-manager/terraform-state-manager/internal/telemetry"
)

func TestRequestID_GeneratesWhenAbsent(t *testing.T) {
	r := gin.New()
	r.Use(RequestID())
	r.GET("/", func(c *gin.Context) { c.Status(http.StatusOK) })

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))

	id := w.Header().Get("X-Request-ID")
	if id == "" {
		t.Fatal("X-Request-ID not set on response")
	}
	if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(id) {
		t.Errorf("generated request ID %q is not 32 hex chars", id)
	}
}

func TestRequestID_HonoursInbound(t *testing.T) {
	r := gin.New()
	r.Use(RequestID())
	var ctxID string
	r.GET("/", func(c *gin.Context) {
		ctxID = c.GetString("request_id")
		c.Status(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Request-ID", "client-supplied-id")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if got := w.Header().Get("X-Request-ID"); got != "client-supplied-id" {
		t.Errorf("response X-Request-ID = %q, want the inbound value echoed", got)
	}
	if ctxID != "client-supplied-id" {
		t.Errorf("context request_id = %q, want client-supplied-id", ctxID)
	}
}

func TestSecurityHeaders(t *testing.T) {
	r := gin.New()
	r.Use(SecurityHeaders())
	r.GET("/", func(c *gin.Context) { c.Status(http.StatusOK) })

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))

	want := map[string]string{
		"X-Content-Type-Options":            "nosniff",
		"X-Frame-Options":                   "DENY",
		"Referrer-Policy":                   "no-referrer",
		"Content-Security-Policy":           "default-src 'none'; frame-ancestors 'none'",
		"X-Permitted-Cross-Domain-Policies": "none",
		"Cross-Origin-Embedder-Policy":      "require-corp",
		"Cross-Origin-Opener-Policy":        "same-origin",
		"Cross-Origin-Resource-Policy":      "same-origin",
	}
	for header, value := range want {
		if got := w.Header().Get(header); got != value {
			t.Errorf("%s = %q, want %q", header, got, value)
		}
	}
}

func TestMetrics_RecordsByRouteTemplate(t *testing.T) {
	r := gin.New()
	r.Use(Metrics())
	r.GET("/api/v1/things/:id", func(c *gin.Context) { c.Status(http.StatusOK) })

	counter := telemetry.HTTPRequestsTotal.WithLabelValues("GET", "/api/v1/things/:id", "200")
	before := testutil.ToFloat64(counter)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/things/abc-123", nil))

	if got := testutil.ToFloat64(counter) - before; got != 1 {
		t.Errorf("http_requests_total delta = %v, want 1 (labelled by route template, not raw URL)", got)
	}
}

func TestMetrics_UnmatchedRoute(t *testing.T) {
	r := gin.New()
	r.Use(Metrics())

	counter := telemetry.HTTPRequestsTotal.WithLabelValues("GET", "unmatched", "404")
	before := testutil.ToFloat64(counter)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/nope", nil))

	if got := testutil.ToFloat64(counter) - before; got != 1 {
		t.Errorf("unmatched-route counter delta = %v, want 1", got)
	}
}

// accessLogRecords runs a request through RequestID()+AccessLog()+handler and
// returns the decoded JSON log records captured from slog's default logger.
func accessLogRecords(t *testing.T, target string, handler gin.HandlerFunc) []map[string]any {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	r := gin.New()
	r.Use(RequestID())
	r.Use(AccessLog())
	r.GET("/api/v1/things/:id", handler)
	r.GET("/health", handler)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, target, nil))

	var records []map[string]any
	for _, line := range bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal(line, &rec); err != nil {
			t.Fatalf("invalid log line %q: %v", line, err)
		}
		records = append(records, rec)
	}
	return records
}

func TestAccessLog_RecordsRequest(t *testing.T) {
	records := accessLogRecords(t, "/api/v1/things/abc-123", func(c *gin.Context) {
		c.Set("user_id", "u1") // what the auth middleware does during c.Next()
		c.Status(http.StatusTeapot)
	})

	if len(records) != 1 {
		t.Fatalf("expected exactly 1 access-log record, got %d", len(records))
	}
	rec := records[0]
	if rec["msg"] != "http_request" {
		t.Errorf("msg = %v, want http_request", rec["msg"])
	}
	if rec["method"] != "GET" {
		t.Errorf("method = %v, want GET", rec["method"])
	}
	if rec["path"] != "/api/v1/things/:id" {
		t.Errorf("path = %v, want the route template /api/v1/things/:id", rec["path"])
	}
	if rec["status"] != float64(http.StatusTeapot) {
		t.Errorf("status = %v, want 418", rec["status"])
	}
	if _, ok := rec["latency_ms"]; !ok {
		t.Error("latency_ms missing")
	}
	id, _ := rec["request_id"].(string)
	if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(id) {
		t.Errorf("request_id = %q, want the 32-hex id set by RequestID()", id)
	}
	if rec["user_id"] != "u1" {
		t.Errorf("user_id = %v, want u1 (set during handler)", rec["user_id"])
	}
}

func TestAccessLog_SkipsProbePaths(t *testing.T) {
	records := accessLogRecords(t, "/health", func(c *gin.Context) { c.Status(http.StatusOK) })
	if len(records) != 0 {
		t.Fatalf("expected no access-log records for /health, got %d", len(records))
	}
}
