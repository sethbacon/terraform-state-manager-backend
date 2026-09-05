// Package telemetry provides application-level observability for the Terraform
// State Manager. Metrics are registered against the default Prometheus registry
// and exposed on a dedicated side-channel HTTP server (see cmd/server) so the
// scrape path stays off the public API ingress.
//
// HTTP metrics use the Gin route template (c.FullPath()) rather than the raw URL
// to avoid unbounded label cardinality from user-supplied path segments.
package telemetry

import (
	"database/sql"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// HTTPRequestsTotal counts processed requests by method, route template, status.
	HTTPRequestsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "http_requests_total",
			Help: "Total number of HTTP requests processed, by method, route template, and status code.",
		},
		[]string{"method", "path", "status"},
	)

	// HTTPRequestDuration records request latency by method and route template.
	HTTPRequestDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "http_request_duration_seconds",
			Help:    "Histogram of HTTP request latencies, by method and route template.",
			Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30},
		},
		[]string{"method", "path"},
	)

	// AppInfo is a build-info gauge (always 1) labelled with version metadata.
	AppInfo = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "app_info",
			Help: "Build information; value is always 1.",
		},
		[]string{"version", "go_version", "build_date"},
	)

	dbConnectionsOpen = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "db_connections_open",
		Help: "Number of open database connections (in use + idle).",
	})
	dbConnectionsInUse = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "db_connections_in_use",
		Help: "Number of database connections currently in use.",
	})
	dbConnectionsIdle = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "db_connections_idle",
		Help: "Number of idle database connections.",
	})

	// driftRunsInFlight exports the scheduler's live view of drift runs
	// currently "dispatched" or "running" -- the same count its in-flight cap
	// (Phase 2 fleet-scale pacing, TSM_DRIFT_MAX_IN_FLIGHT) checks before
	// claiming each due schedule. Sampled once per firing that consults it, so
	// it reflects the freshest count seen during the most recent scheduler
	// poll, whether or not a cap is actually configured.
	driftRunsInFlight = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "tsm_drift_runs_in_flight",
		Help: "Drift runs currently dispatched or running, as last sampled by the scheduler.",
	})

	// schedulerDueBacklog exports, at the end of each scheduler poll, how many
	// of the due schedules that poll read were left unclaimed -- deferred under
	// the in-flight cap, or a claim attempt that failed outright. A schedule
	// claimed by a concurrent replica/poll does not count: it is handled, just
	// not by this poll. A sustained non-zero value means due work is arriving
	// faster than the configured cap can drain it.
	schedulerDueBacklog = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "tsm_scheduler_due_backlog",
		Help: "Due schedules read by the most recent scheduler poll that were not claimed.",
	})

	// driftDispatchTotal counts each schedule firing's terminal result: "ok"
	// (claimed and dispatched successfully), "failed" (claimed but the dispatch
	// itself -- or the row's derived authority -- failed, or the claim attempt
	// errored), or "deferred" (the in-flight cap was reached, so the schedule
	// was never claimed and remains due for a later poll).
	driftDispatchTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "tsm_drift_dispatch_total",
			Help: "Scheduler firings by terminal result: ok, failed, or deferred.",
		},
		[]string{"result"},
	)
)

// SetDriftRunsInFlight records the scheduler's most recent in-flight drift-run
// sample.
func SetDriftRunsInFlight(n int) { driftRunsInFlight.Set(float64(n)) }

// SetSchedulerDueBacklog records how many due schedules the most recent
// scheduler poll left unclaimed.
func SetSchedulerDueBacklog(n int) { schedulerDueBacklog.Set(float64(n)) }

// DriftDispatchResult increments the counter for one schedule firing's
// terminal result ("ok", "failed", or "deferred").
func DriftDispatchResult(result string) { driftDispatchTotal.WithLabelValues(result).Inc() }

// StartDBStatsCollector polls the connection pool every 30 seconds and exports
// its statistics as Prometheus gauges. It returns a stop function that halts the
// collector goroutine; callers should defer it (or invoke it during graceful
// shutdown) so the goroutine does not outlive the pool it observes. The stop
// function is idempotent and safe to call from any goroutine.
func StartDBStatsCollector(database *sql.DB) (stop func()) {
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				stats := database.Stats()
				dbConnectionsOpen.Set(float64(stats.OpenConnections))
				dbConnectionsInUse.Set(float64(stats.InUse))
				dbConnectionsIdle.Set(float64(stats.Idle))
			}
		}
	}()
	var once sync.Once
	return func() { once.Do(func() { close(done) }) }
}
