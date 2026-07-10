package middleware

import (
	"log/slog"
	"time"

	"github.com/gin-gonic/gin"
)

// AccessLog emits one structured log line per request, correlated by the
// X-Request-ID assigned by RequestID() (which must run earlier in the chain).
// Probe and scrape endpoints are excluded to keep the log signal-bearing.
// user_id is present only on authenticated requests: the auth middleware sets
// it during c.Next(), and logging happens in the post-Next phase (the same
// pattern Metrics uses to read the final status).
func AccessLog() gin.HandlerFunc {
	skip := map[string]struct{}{"/health": {}, "/ready": {}, "/metrics": {}}
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()

		if _, ok := skip[c.Request.URL.Path]; ok {
			return
		}
		path := c.FullPath() // route template, not raw URL, matching Metrics
		if path == "" {
			path = "unmatched"
		}
		attrs := []any{
			"method", c.Request.Method,
			"path", path,
			"status", c.Writer.Status(),
			"latency_ms", time.Since(start).Milliseconds(),
			"request_id", c.GetString("request_id"),
			"client_ip", c.ClientIP(),
		}
		if uid := c.GetString("user_id"); uid != "" {
			attrs = append(attrs, "user_id", uid)
		}
		slog.Info("http_request", attrs...)
	}
}
