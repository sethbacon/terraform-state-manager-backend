package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"

	"github.com/terraform-state-manager/terraform-state-manager/internal/config"
)

func init() { gin.SetMode(gin.TestMode) }

// newTestRouter builds a router with OIDC disabled and nil DBs. /health and
// /version don't touch the database, so nil is safe for those endpoints.
func newTestRouter(t *testing.T) *gin.Engine {
	t.Helper()
	router, stop, err := NewRouter(&config.Config{}, nil, nil)
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	t.Cleanup(stop)
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

// readyRecorder drives GET /ready against a ready() handler built from the
// given pools (either may be nil, mirroring the nil-DB test router).
func readyRecorder(t *testing.T, database, identityDB *sql.DB) *httptest.ResponseRecorder {
	t.Helper()
	r := gin.New()
	r.GET("/ready", ready(database, identityDB))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/ready", nil))
	return w
}

// pingableDB returns a sqlmock pool with ping monitoring enabled and an
// expectation for one ping (failing with pingErr when non-nil).
func pingableDB(t *testing.T, pingErr error) *sql.DB {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.MonitorPingsOption(true))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	exp := mock.ExpectPing()
	if pingErr != nil {
		exp.WillReturnError(pingErr)
	}
	return db
}

func TestReadyEndpoint_NilDBs(t *testing.T) {
	// The nil-DB router used across these tests must be able to serve /ready
	// without panicking: nil pools are skipped rather than pinged.
	router := newTestRouter(t)

	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/ready", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 from /ready with nil DBs, got %d", w.Code)
	}
}

func TestReady_BothHealthy(t *testing.T) {
	w := readyRecorder(t, pingableDB(t, nil), pingableDB(t, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestReady_AppDown(t *testing.T) {
	w := readyRecorder(t, pingableDB(t, errors.New("boom")), pingableDB(t, nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", w.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	if body["error"] != "database unreachable" {
		t.Fatalf("error = %q, want \"database unreachable\"", body["error"])
	}
}

func TestReady_IdentityDown(t *testing.T) {
	w := readyRecorder(t, pingableDB(t, nil), pingableDB(t, errors.New("boom")))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", w.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	if body["error"] != "identity database unreachable" {
		t.Fatalf("error = %q, want \"identity database unreachable\"", body["error"])
	}
}
