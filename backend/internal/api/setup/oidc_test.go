package setup

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/terraform-state-manager/terraform-state-manager/internal/config"
	"github.com/terraform-state-manager/terraform-state-manager/internal/crypto"
)

func postJSON(handler gin.HandlerFunc, body string) *httptest.ResponseRecorder {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/x", handler)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	return w
}

const goodOIDCBody = `{"issuer_url":"https://idp.example.com","client_id":"c","client_secret":"s"}`

// Coupled (role_seed_owner=registry): SaveOIDCConfig must refuse with 409 before
// touching identity — the defense-in-depth no-clobber guard. Runs before bind,
// so nil repos are never dereferenced.
func TestSaveOIDCConfig_CoupledReturns409(t *testing.T) {
	cfg := &config.Config{}
	cfg.Suite.RoleSeedOwner = "registry"
	h := NewHandlers(nil, nil, nil, nil, nil, cfg, nil)
	if w := postJSON(h.SaveOIDCConfig, goodOIDCBody); w.Code != http.StatusConflict {
		t.Fatalf("coupled SaveOIDCConfig = %d, want 409 (%s)", w.Code, w.Body.String())
	}
}

// Standalone but no encryption key: must 400 rather than store a plaintext
// secret. Skipped if a key happens to be configured in the test environment.
func TestSaveOIDCConfig_NoEncryptionKeyReturns400(t *testing.T) {
	if crypto.Available() {
		t.Skip("TSM_ENCRYPTION_KEY is configured; this test asserts the no-key path")
	}
	cfg := &config.Config{}
	cfg.Suite.RoleSeedOwner = "self"
	h := NewHandlers(nil, nil, nil, nil, nil, cfg, nil)
	if w := postJSON(h.SaveOIDCConfig, goodOIDCBody); w.Code != http.StatusBadRequest {
		t.Fatalf("no-key SaveOIDCConfig = %d, want 400 (%s)", w.Code, w.Body.String())
	}
}

func TestTestOIDCConfig_MissingFieldsReturns400(t *testing.T) {
	h := NewHandlers(nil, nil, nil, nil, nil, &config.Config{}, nil)
	if w := postJSON(h.TestOIDCConfig, `{}`); w.Code != http.StatusBadRequest {
		t.Fatalf("missing fields TestOIDCConfig = %d, want 400", w.Code)
	}
}
