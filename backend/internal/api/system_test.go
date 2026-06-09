package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/terraform-state-manager/terraform-state-manager/internal/config"
)

func init() { gin.SetMode(gin.TestMode) }

// newTestRouter builds a router with OIDC disabled and nil DBs. /health and
// /version don't touch the database, so nil is safe for those endpoints.
func newTestRouter(t *testing.T) *gin.Engine {
	t.Helper()
	router, err := NewRouter(&config.Config{}, nil, nil)
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	return router
}

func TestHealthEndpoint(t *testing.T) {
	router := newTestRouter(t)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	if body["status"] != "ok" {
		t.Fatalf("expected status ok, got %q", body["status"])
	}
}

func TestVersionEndpoint(t *testing.T) {
	AppVersion = "1.2.3"
	AppBuildDate = "2026-06-08"
	router := newTestRouter(t)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/version", nil)
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	if body["version"] != "1.2.3" {
		t.Fatalf("expected version 1.2.3, got %q", body["version"])
	}
	if body["name"] != "terraform-state-manager" {
		t.Fatalf("unexpected name %q", body["name"])
	}
}
