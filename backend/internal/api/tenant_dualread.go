// tenant_dualread.go is PHASE 2b OF FOUR of
// sethbacon/terraform-state-manager-backend#393, and it is scaffolding with a
// scheduled demolition date: Phase 3 moves the /sources readers onto the scoped
// queries and deletes this file whole.
//
// # What it does, and what it deliberately does not do
//
// When TSM_TENANCY_DUAL_READ is on, the two /sources READ routes run the
// organization-scoped query beside the unscoped query they serve, compare the
// two results, and report the difference. The scoped answer is then thrown away.
// The response is byte-for-byte what it was with the flag off.
//
// That asymmetry is the entire design and it comes straight from migration
// 000033's reasoning. Phase 1 added the partition column and filtered nothing,
// because "a migration that added the column AND started filtering on it would
// be a partial cutover, which is how a deployment ends up half-isolated and
// nobody can say which half". A dual read that served the scoped answer — or
// that failed the request when the two disagreed — would be exactly that
// cutover, arriving through a flag instead of a migration. An operator would see
// sources vanish, or 5xx on some reads and not others, and would roll the flag
// back, destroying the evidence Phase 3 is gated on.
//
// # Divergence is reported, never enforced, and is not by itself a fault
//
// On a deployment with one organization the two reads must agree, and any
// disagreement is a defect in the predicate: Phase 3 would begin hiding rows
// from the tenant that owns them.
//
// On a deployment with more than one, the scoped read returning FEWER rows is
// the correct and expected outcome — it is #393's leak, measured. Those withheld
// rows are precisely what one tenant can read of another's today. Erroring on
// them would report the fix working as an outage.
//
// The one reading that is unconditionally wrong is the scoped read returning a
// row the unscoped read did not. The scoped query is the unscoped query plus a
// conjunct, so that cannot happen unless the two readers have stopped asking the
// same question; it is logged at ERROR and counted separately
// (telemetry.TenantScopeWidened).
//
// # Nothing here may fail the request
//
// Every error path logs and returns. A comparison that cannot complete is an
// observation that was not made — never a served read that turns into a 500,
// which would hand the flag the availability blast radius the flag exists to
// avoid.
package api

import (
	"log/slog"

	"github.com/gin-gonic/gin"

	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
	"github.com/terraform-state-manager/terraform-state-manager/internal/telemetry"
	"github.com/terraform-state-manager/terraform-state-manager/internal/tenantscope"
)

// divergenceIDCap bounds how many source ids one log line names. Ids are not
// secrets — every list endpoint hands them out, which is the point
// internal/tenancy/isolation_integration_test.go makes about GetByID — but an
// unbounded list would let a single request write a log line the size of the
// fleet. Nothing else from the row is logged: a Source carries an encrypted
// credential blob and a connector config, and neither belongs in a log.
const divergenceIDCap = 20

// scopeToCompare returns the scope to compare against, or false when
// there is nothing to compare.
//
// A scope that was never resolved (ok == false — the route was not wired with
// middleware.TenantScope, or the membership lookup failed and that middleware
// declined to invent an answer) is NOT treated as an empty scope here. Comparing
// against it would report every served row as withheld and drown the real signal
// in an artefact of the resolver being down. The middleware has already logged
// the failure at ERROR; this simply declines to make a measurement it cannot
// make.
//
// A scope that WAS resolved and is empty is compared normally: "this caller may
// read nothing" is a real answer and Phase 3 will act on it, so it is exactly
// the case an operator needs to see counted before the flip.
func scopeToCompare(c *gin.Context) (tenantscope.Scope, bool) {
	return tenantscope.FromContext(c)
}

// observeSourceListScope compares the unscoped list read against the scoped one.
//
// It compares the FULL sets, not the page the client asked for. The page is one
// window onto a single ordering, so agreement on the underlying sets implies
// agreement on any page of them — while a page-for-page comparison would leave
// divergence on page seven of a large fleet invisible, and would report ordering
// ties as divergence.
func (h *SourcesHandlers) observeSourceListScope(c *gin.Context) {
	scope, ok := scopeToCompare(c)
	if !ok {
		return
	}
	route := c.FullPath()
	ctx := c.Request.Context()

	unscoped, err := h.repo.List(ctx)
	if err != nil {
		slog.Warn("tenant dual-read: unscoped list failed", "error", err, "path", route)
		return
	}
	scoped, err := h.repo.ListInScope(ctx, scope)
	if err != nil {
		slog.Warn("tenant dual-read: scoped list failed", "error", err, "path", route)
		return
	}

	withheld, widened := diffSourceIDs(unscoped, scoped)
	reportSourceDivergence(c, route, scope, withheld, widened)
}

// observeSourceGetScope compares a single served source against the scoped read
// of the same id.
//
// It runs only when the unscoped read FOUND the row. When it did not there is
// nothing for the scoped read to withhold, and asking anyway would put a second
// query behind every 404 — including the ones a scanner generates. The
// consequence is that the "widened" direction is measured on the list route
// only, which is where a whole-set comparison can see it.
func (h *SourcesHandlers) observeSourceGetScope(c *gin.Context, served *repositories.Source) {
	if served == nil {
		return
	}
	scope, ok := scopeToCompare(c)
	if !ok {
		return
	}
	route := c.FullPath()

	scoped, err := h.repo.GetByIDInScope(c.Request.Context(), served.ID, scope)
	if err != nil {
		slog.Warn("tenant dual-read: scoped get failed", "error", err, "path", route)
		return
	}
	var withheld []string
	if scoped == nil {
		withheld = []string{served.ID}
	}
	reportSourceDivergence(c, route, scope, withheld, nil)
}

// diffSourceIDs returns the ids the unscoped read returned and the scoped read
// did not (withheld), and the ids the scoped read returned and the unscoped read
// did not (widened).
func diffSourceIDs(unscoped, scoped []repositories.Source) (withheld, widened []string) {
	inScope := make(map[string]bool, len(scoped))
	for _, s := range scoped {
		inScope[s.ID] = true
	}
	seen := make(map[string]bool, len(unscoped))
	for _, s := range unscoped {
		seen[s.ID] = true
		if !inScope[s.ID] {
			withheld = append(withheld, s.ID)
		}
	}
	for _, s := range scoped {
		if !seen[s.ID] {
			widened = append(widened, s.ID)
		}
	}
	return withheld, widened
}

// reportSourceDivergence records one completed comparison.
//
// The comparison is counted even when it found nothing, because a divergence
// total of zero and a comparison that never ran are indistinguishable otherwise
// — the lesson tsm_authz_role_drift_compared was added for, and the one that
// decides whether "we saw no divergence" is evidence or a tautology.
func reportSourceDivergence(c *gin.Context, route string, scope tenantscope.Scope, withheld, widened []string) {
	telemetry.TenantScopeDualRead(route, len(withheld), len(widened))
	if len(withheld) == 0 && len(widened) == 0 {
		return
	}

	attrs := []any{
		"path", route,
		"request_id", c.GetString("request_id"),
		"platform_admin", scope.PlatformAdmin,
		"scope_organizations", len(scope.OrgIDs),
	}
	if len(withheld) > 0 {
		attrs = append(attrs, "withheld", len(withheld), "withheld_source_ids", capIDs(withheld))
	}
	if len(widened) > 0 {
		// Impossible by construction: the scoped query is the unscoped query
		// plus a conjunct. A count here means the two readers have stopped
		// asking the same question, and Phase 3 would widen rather than narrow.
		attrs = append(attrs, "widened", len(widened), "widened_source_ids", capIDs(widened))
		slog.Error("tenant dual-read: the scoped read returned rows the unscoped read did not", attrs...)
		return
	}
	slog.Warn("tenant dual-read: the scoped read withheld rows the unscoped read served", attrs...)
}

func capIDs(ids []string) []string {
	if len(ids) <= divergenceIDCap {
		return ids
	}
	return ids[:divergenceIDCap]
}
