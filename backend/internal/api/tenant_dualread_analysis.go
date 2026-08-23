package api

import (
	"log/slog"

	"github.com/gin-gonic/gin"

	"github.com/terraform-state-manager/terraform-state-manager/internal/telemetry"
)

// Dual-read observation for the INHERITED analysis tables (#455).
//
// The sources routes have had this since Phase 2; the dashboard and reports
// routes never did, because they read state_analyses and its siblings rather
// than a partition root, and nobody had written a scoped reader to compare
// against. That is now fixed, and this is what makes the difference measurable.
//
// WHY THIS MATTERS MORE THAN IT LOOKS. 000033's stated Phase 3 go/no-go is that
// "on a single-organization deployment the counters must stay at zero" — but on
// such a deployment the scoped predicate matches exactly what the unscoped one
// matched, so zero divergence is ALSO what a completely broken partition
// reports. The evidence only becomes evidence once a deployment genuinely holds
// two organizations with rows in each. These counters are how that shows up.
//
// It reports and never withholds, exactly as the sources observation does: the
// served answer is unchanged, and a divergence here is the leak being observed
// rather than a fault.

// observeAnalysisScope compares the fleet-wide aggregate the dashboard just
// served against the scoped one for this caller.
//
// It compares TOTALS rather than row sets. The aggregates are the disclosure —
// a resource count, a provider histogram — so counting how many states each read
// summarised is the measurement that matches what the route actually leaks. It
// also keeps the observation to one extra query rather than materialising every
// state row twice.
func (h *SourcesHandlers) observeAnalysisScope(c *gin.Context, servedStates int) {
	scope, ok := scopeToCompare(c)
	if !ok {
		return
	}
	route := c.FullPath()

	scoped, err := h.analysisRepo.TotalsInScope(c.Request.Context(), scope)
	if err != nil {
		slog.Warn("tenant dual-read: scoped analysis totals failed", "error", err, "path", route)
		return
	}

	withheld := servedStates - scoped.States
	if withheld < 0 {
		// The scoped read is the unscoped read plus a join conjunct, so it cannot
		// return MORE. A negative here means the two have stopped asking the same
		// question and a Phase 3 flip would widen rather than narrow.
		telemetry.TenantScopeDualRead(route, 0, -withheld)
		slog.Error("tenant dual-read: the scoped analysis read summarised more states than the unscoped one",
			"path", route,
			"request_id", c.GetString("request_id"),
			"served_states", servedStates,
			"scoped_states", scoped.States,
			"platform_admin", scope.PlatformAdmin,
			"scope_organizations", len(scope.OrgIDs))
		return
	}

	telemetry.TenantScopeDualRead(route, withheld, 0)
	if withheld == 0 {
		return
	}
	// Counts only. The states themselves name a tenant's infrastructure, and the
	// point of the measurement is how MANY would be withheld, not which.
	slog.Info("tenant dual-read: analysis aggregate is fleet-wide",
		"path", route,
		"request_id", c.GetString("request_id"),
		"served_states", servedStates,
		"scoped_states", scoped.States,
		"withheld_states", withheld,
		"platform_admin", scope.PlatformAdmin,
		"scope_organizations", len(scope.OrgIDs))
}
