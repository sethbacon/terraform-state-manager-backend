// Package telemetry provides Prometheus instrumentation and structured logging setup.
package telemetry

import (
	"database/sql"
	"log/slog"
	"os"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// SetupLogger configures the global slog logger with the specified format and level.
func SetupLogger(format, level string) {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}

	opts := &slog.HandlerOptions{Level: lvl}

	var handler slog.Handler
	if format == "json" {
		handler = slog.NewJSONHandler(os.Stdout, opts)
	} else {
		handler = slog.NewTextHandler(os.Stdout, opts)
	}

	slog.SetDefault(slog.New(handler))
}

// dbStatsCollector exports sql.DB pool metrics to Prometheus.
type dbStatsCollector struct {
	db *sql.DB

	maxOpen     *prometheus.Desc
	open        *prometheus.Desc
	inUse       *prometheus.Desc
	idle        *prometheus.Desc
	waitCount   *prometheus.Desc
	waitDurSec  *prometheus.Desc
	maxIdleClosed *prometheus.Desc
	maxLifetimeClosed *prometheus.Desc
}

// StartDBStatsCollector registers a Prometheus collector that periodically
// exports sql.DB pool statistics.
func StartDBStatsCollector(db *sql.DB) {
	c := &dbStatsCollector{
		db: db,
		maxOpen:     prometheus.NewDesc("tsm_db_max_open_connections", "Maximum number of open connections to the database.", nil, nil),
		open:        prometheus.NewDesc("tsm_db_open_connections", "The number of established connections both in use and idle.", nil, nil),
		inUse:       prometheus.NewDesc("tsm_db_in_use_connections", "The number of connections currently in use.", nil, nil),
		idle:        prometheus.NewDesc("tsm_db_idle_connections", "The number of idle connections.", nil, nil),
		waitCount:   prometheus.NewDesc("tsm_db_wait_count_total", "The total number of connections waited for.", nil, nil),
		waitDurSec:  prometheus.NewDesc("tsm_db_wait_duration_seconds_total", "The total time blocked waiting for a new connection.", nil, nil),
		maxIdleClosed: prometheus.NewDesc("tsm_db_max_idle_closed_total", "The total number of connections closed due to SetMaxIdleConns.", nil, nil),
		maxLifetimeClosed: prometheus.NewDesc("tsm_db_max_lifetime_closed_total", "The total number of connections closed due to SetConnMaxLifetime.", nil, nil),
	}
	prometheus.MustRegister(c)
}

func (c *dbStatsCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.maxOpen
	ch <- c.open
	ch <- c.inUse
	ch <- c.idle
	ch <- c.waitCount
	ch <- c.waitDurSec
	ch <- c.maxIdleClosed
	ch <- c.maxLifetimeClosed
}

func (c *dbStatsCollector) Collect(ch chan<- prometheus.Metric) {
	stats := c.db.Stats()
	ch <- prometheus.MustNewConstMetric(c.maxOpen, prometheus.GaugeValue, float64(stats.MaxOpenConnections))
	ch <- prometheus.MustNewConstMetric(c.open, prometheus.GaugeValue, float64(stats.OpenConnections))
	ch <- prometheus.MustNewConstMetric(c.inUse, prometheus.GaugeValue, float64(stats.InUse))
	ch <- prometheus.MustNewConstMetric(c.idle, prometheus.GaugeValue, float64(stats.Idle))
	ch <- prometheus.MustNewConstMetric(c.waitCount, prometheus.CounterValue, float64(stats.WaitCount))
	ch <- prometheus.MustNewConstMetric(c.waitDurSec, prometheus.CounterValue, stats.WaitDuration.Seconds())
	ch <- prometheus.MustNewConstMetric(c.maxIdleClosed, prometheus.CounterValue, float64(stats.MaxIdleClosed))
	ch <- prometheus.MustNewConstMetric(c.maxLifetimeClosed, prometheus.CounterValue, float64(stats.MaxLifetimeClosed))
}

// RecordLatency is a helper to measure latency.
func RecordLatency(start time.Time) time.Duration {
	return time.Since(start)
}
