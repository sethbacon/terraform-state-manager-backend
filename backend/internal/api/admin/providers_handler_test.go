package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// TestProvidersHandler_NoProvidersConfigured verifies the handler returns an
// empty JSON array (not null) and 200 when no auth provider is configured, so
// the data-driven login page renders cleanly.
func TestProvidersHandler_NoProvidersConfigured(t *testing.T) {
	gin.SetMode(gin.TestMode)

	h := &AuthHandlers{} // zero value: no OIDC provider loaded
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/auth/providers", nil)

	h.ProvidersHandler()(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var body struct {
		Providers []map[string]any `json:"providers"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	if body.Providers == nil {
		t.Fatal("providers must be an empty array, not null")
	}
	if len(body.Providers) != 0 {
		t.Errorf("expected 0 providers when none configured, got %d", len(body.Providers))
	}
}
