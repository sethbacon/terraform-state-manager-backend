package middleware

import (
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
