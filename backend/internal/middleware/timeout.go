// timeout.go bounds a handler's request context so a hung or slow-to-respond
// state backend cannot block the handler goroutine — and any per-key lock it
// holds — indefinitely (#263).
package middleware

import (
	"context"
	"time"

	"github.com/gin-gonic/gin"
)

// RequestTimeout returns middleware that caps the request context at d. Every
// statesource connector call reachable from the handler inherits this context
// (conn.Read/Write/List/Delete, transfers, git CloneContext, the AWS/HTTP SDK
// clients), so when d elapses the in-flight backend call is cancelled and the
// handler returns instead of blocking. Chosen generously (a backstop for a
// pathological hang, below the advisory-lock stale TTL), not as an expected
// per-operation latency bound.
func RequestTimeout(d time.Duration) gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx, cancel := context.WithTimeout(c.Request.Context(), d)
		defer cancel()
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
}
