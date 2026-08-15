package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"

	"github.com/terraform-state-manager/terraform-state-manager/internal/auth"
	"github.com/terraform-state-manager/terraform-state-manager/internal/config"
	"github.com/terraform-state-manager/terraform-state-manager/internal/middleware"
)

// This file wires the two highest-value authorization-gated routes with the REAL
// middleware and real auth.Scope constants exactly as router.go does — not the
// scope-injecting stub the rest of the api HTTP tests use — so a regression that
// weakens or removes the gate (e.g. dropping ScopeAdmin from force-unlock, or the
// suite-service-token from audit-ingest) fails a test instead of only being
// caught in production (#267, CWE-862). It mirrors admin_org_routing_test.go's
// approach for the /admin/organizations subtree.

// newForceUnlockAuthzEnv wires DELETE /sources/:id/state/lock behind the real
// middleware.RequireScope(auth.ScopeAdmin) + ForceUnlock, exactly as router.go:223.
// The stub middleware only injects the caller's already-resolved scopes (what the
// auth middleware sets in production); RequireScope makes the real decision.
func newForceUnlockAuthzEnv(t *testing.T, scopes []string) *sourcesEnv {
	t.Helper()
	t.Setenv("TSM_ENCRYPTION_KEY", "0123456789abcdef0123456789abcdef")
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	h := NewSourcesHandlers(db, nil)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		if scopes != nil {
			c.Set("scopes", scopes)
		}
		c.Next()
	})
	v1 := r.Group("/api/v1")
	s := v1.Group("/sources")
	s.DELETE("/:id/state/lock", middleware.RequireScope(auth.ScopeAdmin), h.ForceUnlock())
	return &sourcesEnv{r: r, mock: mock}
}

// TestForceUnlock_NonAdminScopeForbidden proves the force-unlock escape hatch is
// gated on ScopeAdmin specifically: a caller holding powerful but non-admin
// source scopes is still refused at the router layer (403) and never reaches the
// handler (no DB call).
func TestForceUnlock_NonAdminScopeForbidden(t *testing.T) {
	e := newForceUnlockAuthzEnv(t, []string{"state:read", "state:write", "sources:manage"})

	w := e.do(http.MethodDelete, "/api/v1/sources/s1/state/lock?key=app.tfstate", "")
	if w.Code != http.StatusForbidden {
		t.Fatalf("non-admin force-unlock: status = %d (%s), want 403", w.Code, w.Body.String())
	}
	// If the ScopeAdmin gate were dropped, the request (which carries key=) would
	// reach ForceUnlock, query the unexpectant mock, and return 500 — so the 403
	// assertion above is what actually guards the gate.
}

// TestForceUnlock_AdminScopePassesGate proves the gate is wired to admin rather
// than blocking everyone: an admin caller clears RequireScope and reaches the
// handler, which then returns 400 on the omitted key (a business-validation
// response that can only be produced past the authorization gate — a still-gated
// route would have returned 403 here instead).
func TestForceUnlock_AdminScopePassesGate(t *testing.T) {
	e := newForceUnlockAuthzEnv(t, []string{"admin"})

	w := e.do(http.MethodDelete, "/api/v1/sources/s1/state/lock", "")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("admin force-unlock (no key): status = %d (%s), want 400 (past the gate, missing key)", w.Code, w.Body.String())
	}
}

// TestAuditIngest_RequiresSuiteServiceToken wires POST /audit/ingest behind the
// real middleware.RequireSuiteServiceToken exactly as router.go:238-239, and
// proves the server-to-server audit-federation endpoint is unreachable without a
// matching X-Suite-Service-Token: a missing or wrong token is rejected with 401
// before the handler runs, while the correct token clears the gate (nil identity
// DB then yields 503 — notably NOT 401 — proving the handler was reached).
func TestAuditIngest_RequiresSuiteServiceToken(t *testing.T) {
	cfg := &config.Config{Suite: config.SuiteConfig{ServiceToken: "s3cret-suite-token", IdentitySharedStore: true}}
	h := NewAuditIngestHandlers(nil, nil, cfg)

	r := gin.New()
	r.POST("/api/v1/audit/ingest",
		middleware.RequireSuiteServiceToken(cfg.Suite.ServiceToken), h.Ingest())

	do := func(setToken string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/audit/ingest", strings.NewReader(`{"action":"x"}`))
		req.Header.Set("Content-Type", "application/json")
		if setToken != "" {
			req.Header.Set(middleware.SuiteServiceTokenHeader, setToken)
		}
		r.ServeHTTP(w, req)
		return w
	}

	if w := do(""); w.Code != http.StatusUnauthorized {
		t.Fatalf("audit-ingest without suite token: status = %d, want 401", w.Code)
	}
	if w := do("not-the-token"); w.Code != http.StatusUnauthorized {
		t.Fatalf("audit-ingest with wrong suite token: status = %d, want 401", w.Code)
	}
	if w := do("s3cret-suite-token"); w.Code == http.StatusUnauthorized {
		t.Fatalf("audit-ingest with correct suite token must clear the gate, got 401 (%s)", w.Body.String())
	}
}
