package middleware

import (
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const (
	// RequestIDHeader is the HTTP header used to propagate request IDs.
	RequestIDHeader = "X-Request-ID"

	// RequestIDKey is the gin context key under which the request ID is stored.
	RequestIDKey = "request_id"
)

// RequestIDMiddleware returns a gin.HandlerFunc that ensures every request
// carries a unique identifier. If the inbound request already contains an
// X-Request-ID header its value is reused; otherwise a new UUID v4 is
// generated. The identifier is stored in the gin context under the
// "request_id" key and echoed back in the response's X-Request-ID header.
func RequestIDMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		requestID := c.GetHeader(RequestIDHeader)
		if requestID == "" {
			requestID = uuid.New().String()
		}

		c.Set(RequestIDKey, requestID)
		c.Header(RequestIDHeader, requestID)

		c.Next()
	}
}
