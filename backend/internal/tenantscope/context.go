package tenantscope

import "github.com/gin-gonic/gin"

// ContextKey is where the resolved Scope is published on a gin.Context.
//
// A plain string, matching every other value the middleware chain publishes
// ("user_id", "scopes", "auth_method", "request_id"): gin.Context's store is
// map[string]any and a typed key would only be a private wrapper around the same
// string. The name is deliberately not "scope" or "scopes" — the latter is
// already taken by the FLAT, cross-organization scope list, and confusing the
// two is precisely the mistake this package exists to prevent.
const ContextKey = "tenant_scope"

// Store publishes a resolved Scope on the request.
//
// Only middleware.TenantScope should call this. It is exported so the resolution
// and the retrieval live in one package and cannot drift onto two different keys.
func Store(c *gin.Context, s Scope) {
	if c == nil {
		return
	}
	c.Set(ContextKey, s)
}

// FromContext returns the Scope resolved for this request, and whether one was
// resolved at all.
//
// # The second return value is not a convenience, it is the whole safety property
//
// There are two distinct states and a caller MUST NOT conflate them:
//
//   - (Scope{}, true) — a scope WAS resolved and it permits nothing. A caller
//     with no principal, or one holding the required scope in no organization.
//     Reading with it is correct and returns no rows: fail-closed, answered.
//
//   - (Scope{}, false) — no scope was resolved. The route was not wired with
//     middleware.TenantScope, or the membership lookup FAILED and the middleware
//     declined to invent an answer. The question is unanswered, and the zero
//     Scope returned alongside is a safe default rather than a verdict.
//
// Phase 2b has no enforcing caller, so today the difference is only reported (see
// middleware.TenantScope). PHASE 3 MUST TREAT ok == false AS 500, not as an empty
// scope and certainly not as a full one: a route that silently read unscoped
// because the resolver was never wired would be the leak of #393 reintroduced by
// a missing line in the router, which is the least visible way to reintroduce it.
//
// A value stored under the key that is not a Scope is reported as not resolved,
// for the same reason tenantscope.principal treats a non-string user_id as no
// principal: a value that cannot be interpreted cannot authorize anything.
func FromContext(c *gin.Context) (Scope, bool) {
	if c == nil {
		return Scope{}, false
	}
	v, ok := c.Get(ContextKey)
	if !ok {
		return Scope{}, false
	}
	s, ok := v.(Scope)
	if !ok {
		return Scope{}, false
	}
	return s, true
}
