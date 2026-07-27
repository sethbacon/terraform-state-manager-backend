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
)

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
