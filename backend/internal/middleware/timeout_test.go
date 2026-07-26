package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestRequestTimeout_CancelsSlowHandler(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	var deadlineSet bool
	var ctxErr error
	r.GET("/", RequestTimeout(50*time.Millisecond), func(c *gin.Context) {
		_, deadlineSet = c.Request.Context().Deadline()
		// Stand in for a slow backend call that honors the context.
		select {
		case <-time.After(2 * time.Second):
		case <-c.Request.Context().Done():
			ctxErr = c.Request.Context().Err()
		}
		c.Status(http.StatusOK)
	})

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))

	if !deadlineSet {
		t.Error("expected a context deadline to be set on the request")
	}
	if ctxErr == nil {
		t.Error("expected the handler's context to be cancelled once the timeout elapsed")
	}
}
