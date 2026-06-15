package middleware

import (
	"crypto/subtle"
	"net/http"

	"github.com/gin-gonic/gin"
)

// SuiteServiceTokenHeader carries the shared secret a sibling Suite app presents
// to reach cross-app, server-to-server read endpoints (e.g. GET /consumers).
const SuiteServiceTokenHeader = "X-Suite-Service-Token"

// RequireSuiteServiceToken gates an endpoint on a shared suite service token, for
// server-to-server calls from the sibling app that carry no user session. When
// the configured token is empty (the default) the endpoint is effectively
// disabled — every request is rejected — so it stays off until an operator
// provisions a matching token on both apps. The comparison is constant-time.
func RequireSuiteServiceToken(token string) gin.HandlerFunc {
	want := []byte(token)
	return func(c *gin.Context) {
		got := []byte(c.GetHeader(SuiteServiceTokenHeader))
		if len(want) == 0 || subtle.ConstantTimeCompare(got, want) != 1 {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "valid suite service token required"})
			return
		}
		c.Next()
	}
}
