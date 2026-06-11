package telemetry

import (
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/prometheus/client_golang/prometheus"
)

func TestMetricRegistration(t *testing.T) {
	// Importing this package triggers all promauto.New* declarations.
	// These assertions verify every exported metric var was initialized.
	tests := []struct {
		name   string
		metric prometheus.Collector
	}{
		{"HTTPRequestsTotal", HTTPRequestsTotal},
		{"HTTPRequestDuration", HTTPRequestDuration},
		{"AppInfo", AppInfo},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.metric == nil {
				t.Errorf("%s is nil", tt.name)
			}
		})
	}
}

func TestHTTPRequestsTotalLabels(t *testing.T) {
	// Verify we can record with the expected label set without panicking.
	HTTPRequestsTotal.WithLabelValues("GET", "/api/v1/sources", "200").Inc()
}

func TestHTTPRequestDurationLabels(t *testing.T) {
	HTTPRequestDuration.WithLabelValues("GET", "/api/v1/sources").Observe(0.042)
}

func TestAppInfoLabels(t *testing.T) {
	AppInfo.WithLabelValues("1.0.0", "go1.26", "2026-01-01").Set(1)
}

func TestStartDBStatsCollector(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("failed to create sqlmock: %v", err)
	}
	defer db.Close()

	// StartDBStatsCollector should not panic; we don't wait for the goroutine
	// to tick (30s) — the test verifies the function launches cleanly.
	StartDBStatsCollector(db)
}
