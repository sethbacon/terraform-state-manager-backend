package telemetry

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// The standing detector for sethbacon/terraform-suite-identity#206 Phase 3b.
//
// Once authorization reads this application's own role tables, a gap between
// them and the shared identity schema does not surface as an error — it surfaces
// as a principal holding the wrong role, and nothing in the request path says so.
// These series are how it says so. They are set by the periodic comparison in
// cmd/server (approles.CheckDrift), which reports and never corrects.
//
// WHY A GAUGE PER KIND AND NOT ONE TOTAL. The kinds are not interchangeable and
// an alert that adds them up would be unactionable: `missing` is a principal who
// has LOST access they should have (loud, and self-reporting — they will say so),
// `stale` is one who has KEPT access they should not (silent, and the one worth
// paging on), and `mismatched` is the two sides recording different roles.
//
// The `scope_divergent` label and the tsm_authz_role_template_drift gauge are
// RETIRED with the identity.role_templates reads that fed them: role definitions
// are per-app by design now, so "identity's copy differs" stopped being drift —
// see internal/approles/drift.go.
var (
	authzDriftAssignments = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "tsm_authz_role_drift",
			Help: "Role records that disagree between this application's tables and the shared identity schema, by kind.",
		},
		[]string{"kind"},
	)

	authzDriftCompared = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "tsm_authz_role_drift_compared",
			Help: "Role records examined by the most recent comparison. Zero means the comparison found nothing to compare, which is not the same as agreement.",
		},
	)

	authzDriftLastCheck = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "tsm_authz_role_drift_last_check_timestamp_seconds",
			Help: "Unix time of the most recent completed role-drift comparison.",
		},
	)

	authzDriftCheckErrors = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "tsm_authz_role_drift_check_errors_total",
			Help: "Role-drift comparisons that could not complete.",
		},
	)
)

// AuthzDriftObserved records one completed comparison.
//
// The timestamp is set LAST and only on success, so "the drift is zero" and "the
// check has not run since Tuesday" are distinguishable — a stale gauge at zero
// reads exactly like a healthy one, and an alert on the counts alone would go
// quiet at precisely the moment the detector broke. Alert on the age of
// tsm_authz_role_drift_last_check_timestamp_seconds as well as on the counts.
func AuthzDriftObserved(compared, missing, stale, mismatched int) {
	authzDriftAssignments.WithLabelValues("missing").Set(float64(missing))
	authzDriftAssignments.WithLabelValues("stale").Set(float64(stale))
	authzDriftAssignments.WithLabelValues("mismatched").Set(float64(mismatched))
	authzDriftCompared.Set(float64(compared))
	authzDriftLastCheck.SetToCurrentTime()
}

// AuthzDriftCheckFailed records a comparison that could not complete. The gauges
// keep their previous values on purpose: a failed check is not evidence of
// agreement, and zeroing them would manufacture exactly that.
func AuthzDriftCheckFailed() { authzDriftCheckErrors.Inc() }
