package telemetry

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// The standing detector for sethbacon/terraform-state-manager-backend#393
// Phase 2b.
//
// Phase 3 flips TSM's reads onto an organization predicate. Whether that is safe
// is not a code-review question — it is a question about the rows in a
// particular deployment, and the only honest way to answer it is to run both
// reads against real traffic and count where they disagree. That is what
// TSM_TENANCY_DUAL_READ turns on and what these series carry.
//
// WHY A "READS" COUNTER SITS BESIDE THE DIVERGENCE COUNTER, and it is the same
// lesson tsm_authz_role_drift_compared was added for: a divergence total of zero
// and a comparison that never ran look identical from the outside. An operator
// certifying Phase 3 on "no divergence observed" must be able to show that
// something was observed at all — that the flag was on, the routes were hit, and
// the two readers were actually compared.
var (
	tenantScopeDualReads = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "tsm_tenant_scope_dual_reads_total",
			Help: "Reads served unscoped and additionally executed under the caller's organization scope, for comparison. Zero means nothing was compared, which is not the same as agreement.",
		},
		[]string{"route"},
	)

	tenantScopeDivergence = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "tsm_tenant_scope_divergence_total",
			Help: "Reads where the organization-scoped result differed from the unscoped result that was served, by kind.",
		},
		[]string{"route", "kind"},
	)
)

// Divergence kinds. They are not interchangeable and an alert that added them up
// would be unactionable.
const (
	// TenantScopeWithheld: the scoped read returned FEWER rows than the unscoped
	// read that was served. On a single-organization deployment this is a defect
	// in the predicate and Phase 3 would start hiding rows from their owner. On a
	// partitioned deployment it is #393's leak, measured: those rows are what one
	// tenant can currently read of another's.
	TenantScopeWithheld = "withheld"
	// TenantScopeWidened: the scoped read returned rows the unscoped read did
	// not. This is impossible by construction — the scoped query is the unscoped
	// query plus a conjunct — so a non-zero count here means the two readers are
	// no longer asking the same question, and Phase 3 would be a widening rather
	// than a narrowing. It is the loud one.
	TenantScopeWidened = "widened"
)

// TenantScopeDualRead records one completed comparison on a route, along with
// how many rows each kind of disagreement covered. Counts of zero still record
// the comparison, which is the point of the reads counter.
func TenantScopeDualRead(route string, withheld, widened int) {
	tenantScopeDualReads.WithLabelValues(route).Inc()
	if withheld > 0 {
		tenantScopeDivergence.WithLabelValues(route, TenantScopeWithheld).Add(float64(withheld))
	}
	if widened > 0 {
		tenantScopeDivergence.WithLabelValues(route, TenantScopeWidened).Add(float64(widened))
	}
}
