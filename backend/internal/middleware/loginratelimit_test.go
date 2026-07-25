package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestLoginRateLimit_BlocksAfterBudget(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/login", LoginRateLimit(), func(c *gin.Context) { c.Status(http.StatusOK) })

	do := func() int {
		req := httptest.NewRequest(http.MethodPost, "/login", nil)
		req.RemoteAddr = "203.0.113.7:40000"
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code
	}
	// The first loginMaxAttempts requests from one IP pass; the next is 429.
	for i := 0; i < loginMaxAttempts; i++ {
		if code := do(); code != http.StatusOK {
			t.Fatalf("attempt %d: code = %d, want 200", i+1, code)
		}
	}
	if code := do(); code != http.StatusTooManyRequests {
		t.Errorf("attempt %d: code = %d, want 429", loginMaxAttempts+1, code)
	}
}

func TestLoginRateLimit_PerIP(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/login", LoginRateLimit(), func(c *gin.Context) { c.Status(http.StatusOK) })

	doFrom := func(ip string) int {
		req := httptest.NewRequest(http.MethodPost, "/login", nil)
		req.RemoteAddr = ip + ":40000"
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code
	}
	for i := 0; i < loginMaxAttempts; i++ {
		doFrom("198.51.100.1")
	}
	if code := doFrom("198.51.100.1"); code != http.StatusTooManyRequests {
		t.Fatalf("exhausted IP: code = %d, want 429", code)
	}
	// A different IP has its own budget.
	if code := doFrom("198.51.100.2"); code != http.StatusOK {
		t.Errorf("second IP: code = %d, want 200 (limit is per-IP)", code)
	}
}
