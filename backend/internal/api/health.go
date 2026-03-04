// Package api provides health, readiness, and version check handlers for the
// Terraform State Manager backend. These endpoints are unauthenticated and
// intended for use by load balancers, Kubernetes probes, and monitoring tools.
package api

import (
	"database/sql"
	"net/http"
	"runtime"
	"time"

	"github.com/gin-gonic/gin"
)

// healthCheckHandler returns the health status of the service by verifying
// database connectivity. It is intended as a lightweight liveness probe.
func healthCheckHandler(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"status":    "ok",
			"timestamp": time.Now().UTC().Format(time.RFC3339),
		})
	}
}

// readinessHandler returns the readiness status of the service. Unlike the
// liveness probe (/health), readiness checks are used by orchestrators to
// decide whether the instance should receive traffic.
func readinessHandler(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		if err := db.Ping(); err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{
				"status": "not ready",
				"error":  err.Error(),
			})
			return
		}

		c.JSON(http.StatusOK, gin.H{
			"status": "ready",
		})
	}
}

// versionHandler returns the current API version and build information.
func versionHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"version":     "0.1.0",
			"api_version": "v1",
			"go_version":  runtime.Version(),
		})
	}
}
