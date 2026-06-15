package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestRequireSuiteServiceToken(t *testing.T) {
	gin.SetMode(gin.TestMode)
	run := func(configured, header string) int {
		r := gin.New()
		r.GET("/x", RequireSuiteServiceToken(configured), func(c *gin.Context) { c.Status(http.StatusOK) })
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		if header != "" {
			req.Header.Set(SuiteServiceTokenHeader, header)
		}
		r.ServeHTTP(w, req)
		return w.Code
	}

	if got := run("", "anything"); got != http.StatusUnauthorized {
		t.Errorf("unset token must disable the endpoint, got %d", got)
	}
	if got := run("s3cr3t", ""); got != http.StatusUnauthorized {
		t.Errorf("missing header must be 401, got %d", got)
	}
	if got := run("s3cr3t", "wrong"); got != http.StatusUnauthorized {
		t.Errorf("wrong token must be 401, got %d", got)
	}
	if got := run("s3cr3t", "s3cr3t"); got != http.StatusOK {
		t.Errorf("matching token must pass, got %d", got)
	}
}
