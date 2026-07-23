package telemetry

import (
	"sort"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// workerLastTick exports each background loop's last tick so a wedged or
	// panicked worker goroutine is visible even while the process (and its
	// metrics port) stays up — TSMTargetDown alone cannot see that.
	workerLastTick = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "tsm_worker_last_tick_timestamp_seconds",
			Help: "Unix time of each background worker loop's most recent tick.",
		},
		[]string{"worker"},
	)

	// sourceLastSync exports per-source sync freshness (mirrors
	// source_sync_status.last_sync_at, without needing a DB query to alert on).
	sourceLastSync = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "tsm_source_last_sync_timestamp_seconds",
			Help: "Unix time of each state source's most recent completed sync cycle.",
		},
		[]string{"source_id", "source_name"},
	)

	// sourceSyncReadErrors exports each source's read-error count from its most
	// recent sync cycle. Series for deleted sources go stale at their last value.
	sourceSyncReadErrors = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "tsm_source_sync_read_errors",
			Help: "Read errors in each state source's most recent sync cycle.",
		},
		[]string{"source_id", "source_name"},
	)
)

// SourceSynced records the outcome of one source's sync cycle.
func SourceSynced(sourceID, sourceName string, readErrors int) {
	sourceLastSync.WithLabelValues(sourceID, sourceName).SetToCurrentTime()
	sourceSyncReadErrors.WithLabelValues(sourceID, sourceName).Set(float64(readErrors))
}

// workerStatus tracks one registered periodic loop for the in-process health
// check (/ready); the Prometheus gauge above serves out-of-process alerting.
type workerStatus struct {
	last     time.Time
	interval time.Duration
}

var (
	workerMu sync.Mutex
	workers  = map[string]*workerStatus{}
)

// RegisterWorker declares a periodic worker loop and its expected tick
// interval. Registration counts as a first tick, so a fresh boot has a full
// staleness budget before /ready can fail. Only worker-enabled replicas
// register anything, so API-only replicas are unaffected.
func RegisterWorker(name string, interval time.Duration) {
	workerMu.Lock()
	defer workerMu.Unlock()
	workers[name] = &workerStatus{last: time.Now(), interval: interval}
	workerLastTick.WithLabelValues(name).SetToCurrentTime()
}

// UnregisterWorker removes a worker from the health check (test cleanup).
func UnregisterWorker(name string) {
	workerMu.Lock()
	defer workerMu.Unlock()
	delete(workers, name)
}

// WorkerTick records a loop tick for name. Unregistered names are ignored
// entirely (no gauge either), so code paths shared with API-only replicas —
// e.g. a user-triggered sync — don't fabricate worker series there.
func WorkerTick(name string) {
	workerMu.Lock()
	defer workerMu.Unlock()
	if w, ok := workers[name]; ok {
		w.last = time.Now()
		workerLastTick.WithLabelValues(name).SetToCurrentTime()
	}
}

// staleAfter is how far a worker's last tick may lag its expected interval
// before it is reported stale: three missed ticks, with a floor so short
// intervals (the 60s scheduler) tolerate a transient slow cycle.
func staleAfter(interval time.Duration) time.Duration {
	d := 3 * interval
	if d < 2*time.Minute {
		d = 2 * time.Minute
	}
	return d
}

// StaleWorkers returns the names of registered workers whose last tick is
// older than their staleness budget, sorted for stable output. Empty when
// every loop is live (or none are registered).
func StaleWorkers(now time.Time) []string {
	workerMu.Lock()
	defer workerMu.Unlock()
	var stale []string
	for name, w := range workers {
		if now.Sub(w.last) > staleAfter(w.interval) {
			stale = append(stale, name)
		}
	}
	sort.Strings(stale)
	return stale
}
