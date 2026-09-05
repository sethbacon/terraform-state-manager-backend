package telemetry

import (
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
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

// TestSetDriftRecordsOpen pins the Phase 2 metric Phase 4a implements:
// tsm_drift_records_open{severity}, refreshed on the reconciler tick.
func TestSetDriftRecordsOpen(t *testing.T) {
	SetDriftRecordsOpen("critical", 2)
	SetDriftRecordsOpen("warning", 5)
	if got := testutil.ToFloat64(driftRecordsOpen.WithLabelValues("critical")); got != 2 {
		t.Errorf("tsm_drift_records_open{severity=critical} = %v, want 2", got)
	}
	if got := testutil.ToFloat64(driftRecordsOpen.WithLabelValues("warning")); got != 5 {
		t.Errorf("tsm_drift_records_open{severity=warning} = %v, want 5", got)
	}
}

func TestStartDBStatsCollector(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("failed to create sqlmock: %v", err)
	}
	defer db.Close()

	// StartDBStatsCollector should not panic and should return a stop function
	// that halts the collector goroutine; the returned stop must be safe to call
	// more than once (idempotent).
	stop := StartDBStatsCollector(db)
	stop()
	stop()
}
