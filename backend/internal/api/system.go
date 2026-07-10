package api

import (
	"database/sql"
	"net/http"

	"github.com/gin-gonic/gin"
)

// AppVersion and AppBuildDate are set by main at startup from ldflags-injected
// build metadata and surfaced via GET /api/v1/version.
var (
	AppVersion   = "dev"
	AppBuildDate = "unknown"
)

// health reports process liveness; it does not touch dependencies.
func health(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// ready reports readiness, verifying both the app and identity databases are
// reachable — they are separate pools even in the default single-database
// topology (see main), so a down identity store must fail readiness too. Nil
// pools are skipped so the nil-DB test router stays usable.
func ready(database, identityDB *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		if database != nil {
			if err := database.PingContext(c.Request.Context()); err != nil {
				c.JSON(http.StatusServiceUnavailable, gin.H{"status": "unavailable", "error": "database unreachable"})
				return
			}
		}
		if identityDB != nil {
			if err := identityDB.PingContext(c.Request.Context()); err != nil {
				c.JSON(http.StatusServiceUnavailable, gin.H{"status": "unavailable", "error": "identity database unreachable"})
				return
			}
		}
		c.JSON(http.StatusOK, gin.H{"status": "ready"})
	}
}

// version returns build metadata.
func version(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"name":       "terraform-state-manager",
		"version":    AppVersion,
		"build_date": AppBuildDate,
	})
}
