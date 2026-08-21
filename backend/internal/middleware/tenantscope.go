package middleware

import (
	"log/slog"

	"github.com/gin-gonic/gin"

	"github.com/terraform-state-manager/terraform-state-manager/internal/auth"
	"github.com/terraform-state-manager/terraform-state-manager/internal/tenantscope"
)

// TenantScope resolves the caller's tenant scope for the scope this route
// requires, and publishes it on the request.
//
// PHASE 2b OF FOUR of sethbacon/terraform-state-manager-backend#393. Phase 2a
// built internal/tenantscope — the type in which "which organizations is this
// request for?" can be asked. This is what asks it, once per request, so that a
// handler has an answer without every handler re-deriving one.
//
// # It pairs with RequireScope, and the pairing is load-bearing
//
// tenantscope.Resolve returns the organizations in which the caller was verified
// to hold `required` — not the organizations they belong to. So the scope handed
// to a route is only meaningful for the authority that route demands, and this
// middleware must be registered beside the route's own RequireScope, carrying the
// SAME auth.Scope.
//
// THAT IS WHY IT IS NOT REGISTERED ON THE /sources GROUP. A single group-level
// registration would have to name one scope, and the group mixes state:read
// reads with sources:manage writes. Resolving once with state:read would hand
// every write route a tenancy derived from READ authority — and in Phase 3, when
// the scope becomes the predicate, that is a caller editing a source in an
// organization where they may only look at one. The flat RequireScope(sources:manage)
// would not catch it: holding sources:manage in ANY organization satisfies a flat
// check, which is the exact defect #393 is about. One line in a route group is a
// cheap way to rebuild the leak inside the mechanism meant to close it.
//
// # A resolver failure does not abort, in this phase
//
// GUARD tenant-scope-observe-only (#393). Resolve reports a FAILED lookup as an
// error precisely so a caller can refuse to serve rather than guess. This
// middleware deliberately does not refuse yet, and the reason is the one
// migration 000033 gives for keeping Phase 1 non-filtering: a partial cutover is
// how a deployment ends up half-isolated and nobody can say which half.
//
// In Phase 2b nothing reads the scope to decide anything — the served rows come
// from the same unscoped queries as before. Aborting here would therefore take a
// sources request down for a membership-store hiccup that cannot affect a single
// byte of the response: an availability regression contributed by a phase whose
// whole contract is that it changes nothing that is returned, and one an operator
// would rationally roll back, taking the equivalence evidence with it.
//
// The distinction is not discarded, it is CARRIED: an unresolved scope is stored
// as nothing at all, so tenantscope.FromContext reports ok == false, which is a
// different fact from "resolved, and permits nothing" (ok == true, Scope.Empty()).
// PHASE 3 MUST TURN ok == false INTO A 500 at the reading callers. Until then the
// failure is logged at ERROR so a resolver that cannot answer is visible BEFORE
// it becomes load-bearing, rather than on the day it starts deciding reads.
//
// # Cost
//
// One membership query per request on the routes it is registered on
// (approles.Members.OrgScopeForUser), plus a platform_admins lookup for
// session-authenticated callers. It is deliberately live rather than cached, for
// the reason tenantscope.PlatformAdmins gives: a carrier row removed must stop
// elevating on the next request, not when the holder's longest session expires.
func TenantScope(memberships tenantscope.Memberships, admins tenantscope.PlatformAdmins, required auth.Scope) gin.HandlerFunc {
	return func(c *gin.Context) {
		scope, err := tenantscope.Resolve(c, memberships, admins, required)
		if err != nil {
			// No scope is stored: FromContext reports ok == false, and the
			// zero Scope it returns alongside permits nothing.
			slog.Error("tenant scope could not be resolved",
				"error", err,
				"path", c.FullPath(),
				"required_scope", string(required),
				"request_id", c.GetString("request_id"))
			c.Next()
			return
		}
		tenantscope.Store(c, scope)
		c.Next()
	}
}
