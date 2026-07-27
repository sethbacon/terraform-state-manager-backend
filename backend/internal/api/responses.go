package api

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// serverError replies with a generic 500 body carrying msg (safe to expose to the
// client) while recording err on the gin context via c.Error. The AccessLog
// middleware emits c.Errors at Error level when the status is >= 500, so the
// underlying cause reaches the server-side log keyed by request_id without ever
// appearing in the response. Use this at any internal-fault site instead of a
// bare c.JSON(http.StatusInternalServerError, ...) so a 500 is never a black box.
func serverError(c *gin.Context, err error, msg string) {
	if err != nil {
		_ = c.Error(err)
	}
	c.JSON(http.StatusInternalServerError, gin.H{"error": msg})
}

// upstreamError is serverError for non-500 faults: it records the raw err
// server-side (via c.Error, surfaced in the request-keyed access log) but replies
// with the given status and a generic, client-safe msg. Use it at connector /
// backend failure sites (e.g. 502) so a driver/SDK error string — which can carry
// endpoints, hostnames, or other internal detail — never reaches the client
// (#286, CWE-209), while keeping the status more accurate than a bare 500.
func upstreamError(c *gin.Context, status int, err error, msg string) {
	if err != nil {
		_ = c.Error(err)
	}
	c.JSON(status, gin.H{"error": msg})
}
