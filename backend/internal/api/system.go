package api

import (
	"database/sql"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/terraform-state-manager/terraform-state-manager/internal/telemetry"
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

// staleWorkers reports stalled background loops; a var so tests can inject
// staleness without waiting out a real interval.
var staleWorkers = telemetry.StaleWorkers

// ready reports readiness, verifying both the app and identity databases are
// reachable — they are separate pools even in the default single-database
// topology (see main), so a down identity store must fail readiness too. Nil
// pools are skipped so the nil-DB test router stays usable.
//
// On a worker-enabled replica it also fails when any registered background
// loop stopped ticking (statesync/scheduler/reconcilers) — a wedged or
// panicked worker goroutine leaves the process up, so DB pings alone would
// keep reporting ready while sync silently stops. API-only replicas register
// no workers, so this check is a no-op there.
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
		if stale := staleWorkers(time.Now()); len(stale) > 0 {
			c.JSON(http.StatusServiceUnavailable, gin.H{
				"status": "unavailable",
				"error":  "background workers stalled: " + strings.Join(stale, ", "),
			})
			return
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
