package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
)

type fakeSetupStore struct {
	completed bool
	pending   bool
	hash      string
}

func (f fakeSetupStore) IsSetupCompleted(context.Context) (bool, error)       { return f.completed, nil }
func (f fakeSetupStore) HasPendingFeatureSetup(context.Context) (bool, error) { return f.pending, nil }
func (f fakeSetupStore) GetSetupTokenHash(context.Context) (string, error)    { return f.hash, nil }

func bhash(t *testing.T, tok string) string {
	t.Helper()
	h, err := bcrypt.GenerateFromPassword([]byte(tok), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	return string(h)
}

func setupRouter(store setupStatusStore) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/setup/x", SetupTokenMiddleware(store), func(c *gin.Context) { c.Status(http.StatusOK) })
	return r
}

func do(r *gin.Engine, authHeader string) int {
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/setup/x", nil)
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	r.ServeHTTP(w, req)
	return w.Code
}

func TestSetupTokenMiddleware_ValidToken(t *testing.T) {
	tok := "tsm_setup_good"
	if code := do(setupRouter(fakeSetupStore{hash: bhash(t, tok)}), "SetupToken "+tok); code != http.StatusOK {
		t.Fatalf("valid token: got %d, want 200", code)
	}
}

func TestSetupTokenMiddleware_InvalidToken(t *testing.T) {
	if code := do(setupRouter(fakeSetupStore{hash: bhash(t, "tsm_setup_real")}), "SetupToken wrong"); code != http.StatusUnauthorized {
		t.Fatalf("invalid token: got %d, want 401", code)
	}
}

func TestSetupTokenMiddleware_MissingOrMalformedHeader(t *testing.T) {
	r := setupRouter(fakeSetupStore{hash: bhash(t, "x")})
	if code := do(r, ""); code != http.StatusUnauthorized {
		t.Errorf("missing header: got %d, want 401", code)
	}
	if code := do(r, "Bearer x"); code != http.StatusUnauthorized {
		t.Errorf("wrong scheme: got %d, want 401", code)
	}
}

func TestSetupTokenMiddleware_CompletedIs403(t *testing.T) {
	if code := do(setupRouter(fakeSetupStore{completed: true, hash: bhash(t, "x")}), "SetupToken x"); code != http.StatusForbidden {
		t.Fatalf("completed setup: got %d, want 403", code)
	}
}

func TestSetupTokenMiddleware_CompletedButPendingPasses(t *testing.T) {
	tok := "tsm_setup_good"
	if code := do(setupRouter(fakeSetupStore{completed: true, pending: true, hash: bhash(t, tok)}), "SetupToken "+tok); code != http.StatusOK {
		t.Fatalf("completed+pending: got %d, want 200", code)
	}
}

func TestSetupTokenMiddleware_NoTokenGeneratedIs403(t *testing.T) {
	if code := do(setupRouter(fakeSetupStore{hash: ""}), "SetupToken x"); code != http.StatusForbidden {
		t.Fatalf("no token generated: got %d, want 403", code)
	}
}

func TestSetupTokenMiddleware_RateLimited(t *testing.T) {
	r := setupRouter(fakeSetupStore{hash: bhash(t, "tsm_setup_real")}) // one shared limiter
	var last int
	for i := 0; i < setupMaxAttempts+1; i++ {
		last = do(r, "SetupToken wrong")
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("after %d attempts: got %d, want 429", setupMaxAttempts+1, last)
	}
}
