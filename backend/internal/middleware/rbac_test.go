package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/terraform-state-manager/terraform-state-manager/internal/auth"
)

// scopeRouter wires a middleware that injects the given scopes into the request
// context (standing in for AuthMiddleware) ahead of the handler under test.
func scopeRouter(scopes []string, requirement gin.HandlerFunc) *gin.Engine {
	r := gin.New()
	r.Use(func(c *gin.Context) {
		if scopes != nil {
			c.Set("scopes", scopes)
		}
		c.Next()
	})
	r.Use(requirement)
	r.GET("/", func(c *gin.Context) { c.Status(http.StatusOK) })
	return r
}

func runScoped(t *testing.T, scopes []string, requirement gin.HandlerFunc) int {
	t.Helper()
	w := httptest.NewRecorder()
	scopeRouter(scopes, requirement).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	return w.Code
}

func TestRequireScope(t *testing.T) {
	tests := []struct {
		name   string
		scopes []string
		scope  auth.Scope
		want   int
	}{
		{"exact match", []string{"state:read"}, auth.ScopeStateRead, http.StatusOK},
		{"admin wildcard", []string{"admin"}, auth.ScopeSourcesManage, http.StatusOK},
		{"write implies read", []string{"state:write"}, auth.ScopeStateRead, http.StatusOK},
		{"missing scope", []string{"state:read"}, auth.ScopeStateWrite, http.StatusForbidden},
		{"empty scopes", []string{}, auth.ScopeStateRead, http.StatusForbidden},
		{"no auth context", nil, auth.ScopeStateRead, http.StatusForbidden},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := runScoped(t, tt.scopes, RequireScope(tt.scope)); got != tt.want {
				t.Errorf("status = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestRequireAnyScope(t *testing.T) {
	tests := []struct {
		name   string
		scopes []string
		any    []auth.Scope
		want   int
	}{
		{"matches one", []string{"state:drift"}, []auth.Scope{auth.ScopeStateRead, auth.ScopeStateDrift}, http.StatusOK},
		{"admin matches", []string{"admin"}, []auth.Scope{auth.ScopeStateTransfer}, http.StatusOK},
		{"matches none", []string{"state:execute"}, []auth.Scope{auth.ScopeStateRead, auth.ScopeStateDrift}, http.StatusForbidden},
		{"no auth context", nil, []auth.Scope{auth.ScopeStateRead}, http.StatusForbidden},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := runScoped(t, tt.scopes, RequireAnyScope(tt.any...)); got != tt.want {
				t.Errorf("status = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestRequireScope_WrongTypeInContext(t *testing.T) {
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set("scopes", "not-a-slice"); c.Next() })
	r.Use(RequireScope(auth.ScopeStateRead))
	r.GET("/", func(c *gin.Context) { c.Status(http.StatusOK) })

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 for malformed scopes context", w.Code)
	}
}
