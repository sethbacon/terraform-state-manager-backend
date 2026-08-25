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
		status := c.Writer.Status()
		attrs := []any{
			"method", c.Request.Method,
			"path", path,
			"status", status,
			"latency_ms", time.Since(start).Milliseconds(),
			"request_id", c.GetString("request_id"),
			"client_ip", c.ClientIP(),
		}
		if uid := c.GetString("user_id"); uid != "" {
			attrs = append(attrs, "user_id", uid)
		}
		// A 5xx means an internal fault the client sees only as a generic body.
		// Surface the cause (attached by handlers via c.Error / serverError) in the
		// server-side log so a request_id can be traced to a real error, and log at
		// Error level so 5xx is alertable. The response body is unchanged.
		// A request the CLIENT abandoned is not a server fault, whatever status
		// the handler managed to write before noticing (#487).
		//
		// api.serverError already answers 499 for the paths that go through it,
		// which is most of them. This second check exists for the handlers that
		// write a 500 directly: it cannot change the status they already sent,
		// but it does keep a client disconnect out of the Error stream, which is
		// where the actual harm was -- an ERROR line on every single login makes
		// the level meaningless for the faults it is supposed to surface.
		//
		// Read from the REQUEST context, not from c.Errors: the handler may have
		// swallowed the cancellation without attaching anything.
		if c.Request != nil && c.Request.Context().Err() != nil {
			attrs = append(attrs, "client_disconnected", true)
			if len(c.Errors) > 0 {
				attrs = append(attrs, "error", c.Errors.String())
			}
			slog.Info("http_request", attrs...)
			return
		}
		if status >= 500 {
			if len(c.Errors) > 0 {
				attrs = append(attrs, "error", c.Errors.String())
			}
			slog.Error("http_request", attrs...)
			return
		}
		slog.Info("http_request", attrs...)
	}
}
