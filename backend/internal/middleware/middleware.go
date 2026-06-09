// Package middleware holds the Gin HTTP middleware chain. The Phase 0 scaffold
// ships the cross-cutting basics (request IDs, security headers, request metrics);
// authentication, RBAC, audit, rate limiting, and CSRF are layered in as those
// features land, mirroring the sibling registry backend's middleware stack.
package middleware

import (
	"crypto/rand"
	"encoding/hex"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/terraform-state-manager/terraform-state-manager/internal/telemetry"
)

// RequestID assigns each request a stable identifier, honouring an inbound
// X-Request-ID header when present and echoing it back on the response.
func RequestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.GetHeader("X-Request-ID")
		if id == "" {
			id = randomID()
		}
		c.Set("request_id", id)
		c.Header("X-Request-ID", id)
		c.Next()
	}
}

// SecurityHeaders sets conservative default security response headers.
func SecurityHeaders() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("X-Content-Type-Options", "nosniff")
		c.Header("X-Frame-Options", "DENY")
		c.Header("Referrer-Policy", "strict-origin-when-cross-origin")
		c.Next()
	}
}

// Metrics records request counts and latency, labelled by the matched route
// template (not the raw URL) to bound Prometheus label cardinality.
func Metrics() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()

		path := c.FullPath()
		if path == "" {
			path = "unmatched"
		}
		status := strconv.Itoa(c.Writer.Status())
		telemetry.HTTPRequestsTotal.WithLabelValues(c.Request.Method, path, status).Inc()
		telemetry.HTTPRequestDuration.WithLabelValues(c.Request.Method, path).Observe(time.Since(start).Seconds())
	}
}

func randomID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "unknown"
	}
	return hex.EncodeToString(b)
}
