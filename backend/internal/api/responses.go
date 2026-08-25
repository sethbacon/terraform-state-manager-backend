package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
)

// serverError replies with a generic 500 body carrying msg (safe to expose to the
// client) while recording err on the gin context via c.Error -- unless the
// client abandoned the request, in which case it answers 499 and records
// nothing (#487). The AccessLog
// middleware emits c.Errors at Error level when the status is >= 500, so the
// underlying cause reaches the server-side log keyed by request_id without ever
// appearing in the response. Use this at any internal-fault site instead of a
// bare c.JSON(http.StatusInternalServerError, ...) so a 500 is never a black box.
// StatusClientClosedRequest is nginx's 499: the client went away before the
// server finished answering. It is not an IANA code, but it is what ingress
// controllers and log aggregators already understand, and it keeps a client
// disconnect out of the 5xx band that alerting and the access log's Error level
// watch.
const StatusClientClosedRequest = 499

// clientWentAway reports whether err is this request being ABANDONED BY THE
// CLIENT, rather than a server-side failure that happens to involve a
// cancelled context.
//
// Both conditions are required. errors.Is alone is not enough: a context the
// SERVER cancelled -- a sub-context behind an internal timeout, say -- also
// yields context.Canceled, and that genuinely is a server fault which must keep
// its 500. Consulting the request context as well distinguishes "the caller
// hung up" from "we gave up", which are the same error value and opposite
// answers.
func clientWentAway(c *gin.Context, err error) bool {
	return err != nil &&
		errors.Is(err, context.Canceled) &&
		c.Request != nil &&
		c.Request.Context().Err() != nil
}

func serverError(c *gin.Context, err error, msg string) {
	// A request the CLIENT abandoned is not a server fault, and must not be
	// reported as one (#487). The frontend fires /auth/me as the OIDC callback
	// is still settling and then aborts it, so EVERY login produced a 500 and
	// an ERROR line for a condition that is ordinary client behaviour -- in the
	// one log stream where ERROR is supposed to mean something is wrong.
	//
	// No body is written: nobody is listening, and the error is not attached to
	// c.Errors either, because the access log surfaces c.Errors precisely to
	// explain 5xx responses and this is not one.
	if clientWentAway(c, err) {
		// AbortWithStatus, not Status: gin defers the header write, so Status
		// alone leaves the recorder (and a real connection) reporting 200.
		c.AbortWithStatus(StatusClientClosedRequest)
		return
	}
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
