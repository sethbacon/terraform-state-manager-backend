// Package tenantscope adapts this application's HTTP requests to the suite's
// shared tenancy resolver.
//
// THE LOGIC USED TO LIVE HERE AND NO LONGER DOES. terraform-registry-backend
// carried the same package solving the same class, neither imported the other,
// and by the time both were written they had drifted into two different
// authority models under one type name. It is now
// identity/tenantscope in terraform-suite-identity (its #247), which
// sethbacon/terraform-suite-identity#206 makes the rule for: the library owns
// mechanism, the app owns policy.
//
// What remains here is the two things that genuinely belong to this repository:
// pulling a Principal out of a *gin.Context, and stating THIS application's
// policy. Both are small and both are the parts a shared package must not
// assume.
//
// # This application's policy, and why each answer is what it is
//
// AdminsApplyToAPIKeys is OFF. A key minted for automation must not inherit its
// owner's platform authority; internal/middleware/auth.go already caps a key's
// scopes by its owner's CURRENT ones for the same reason (#223).
//
// KeyBindsOrganization is OFF, and this one is temporary. terraform-registry
// turns it ON, deliberately, after its #719 found that a userless organization
// service key had no memberships to resolve and so received empty lists and
// refusals on its OWN organization. That fix is right there and wrong here,
// because internal/api/apikeys.go:mintKey stamps EVERY key with the deployment's
// default organization whoever owns it — so the column is a constant, and
// binding to it would place every key in one organization. It becomes correct
// once #436 makes that stamp mean something. #438 tracks the gap in the
// meantime.
//
// PlatformAdmins is the live carrier, not a flat scope. In registry `admin` IS
// the platform-wide wildcard; here it is granted per organization through an
// admin-bearing role template and merely SURFACES flat, so reading it off the
// request would hand every single-organization admin the whole deployment.
package tenantscope

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/gin-gonic/gin"
	idtenantscope "github.com/sethbacon/terraform-suite-identity/identity/tenantscope"

	"github.com/terraform-state-manager/terraform-state-manager/internal/auth"
	"github.com/terraform-state-manager/terraform-state-manager/internal/platformadmin"
)

// Scope, Memberships and PlatformAdmins are ALIASES rather than definitions, so
// every existing caller — the repositories' scoped readers, the dual-read
// comparator, the middleware — compiles unchanged and interoperates with the
// shared package without a conversion at the seam. A conversion is where a
// PlatformAdmin flag gets dropped.
type (
	Scope          = idtenantscope.Scope
	Memberships    = idtenantscope.Memberships
	PlatformAdmins = idtenantscope.PlatformAdmins
)

// Resolve returns the organizations this request may reach.
//
// It is the same call it always was; the decision now lives in the shared
// package and this states the policy it is made under.
func Resolve(c *gin.Context, memberships Memberships, admins PlatformAdmins, required auth.Scope) (Scope, error) {
	if c == nil {
		return Scope{}, nil
	}
	resolver := idtenantscope.Resolver{
		Memberships:    memberships,
		ReadWritePairs: auth.ReadWritePairs(),
		// Both deliberately left at their zero value; see the package comment.
		AdminsApplyToAPIKeys: false,
		KeyBindsOrganization: false,
	}
	if admins != nil {
		resolver.Admins = notConfiguredShim{inner: admins}
	}
	return resolver.Resolve(requestContext(c), principalOf(c), string(required))
}

// ActingOrganization is the organization a WRITE belongs to: the one named by
// the request, verified against a scope this server resolved.
//
// Separate from Resolve because they answer different questions. Resolve returns
// a SET, which is the right answer for a read and no answer at all for a write.
func ActingOrganization(c *gin.Context, scope Scope) (string, error) {
	selected := ""
	if c != nil && c.Request != nil {
		selected = c.GetHeader(idtenantscope.ActingOrganizationHeader)
	}
	return idtenantscope.Resolver{}.ActingOrganization(scope, selected)
}

// notConfiguredShim translates this repository's "carrier was never constructed"
// sentinel into the one the shared resolver recognises.
//
// WITHOUT THIS THE FALL-THROUGH SILENTLY BECOMES A HARD ERROR. The shared
// resolver treats an absent carrier as WITHHOLDING authority — it defers to
// memberships rather than denying — and it recognises that by matching its own
// ErrAdminsNotConfigured or identity's platformadmin.ErrNotConfigured. This
// repository's internal/platformadmin declares a THIRD, textually different
// sentinel of its own, which neither matches. A deployment with no carrier wired
// would have started answering 500 on every scoped read instead of resolving
// memberships, and nothing about the call site would have looked wrong.
//
// Both errors are wrapped, so errors.Is still finds the original.
type notConfiguredShim struct{ inner PlatformAdmins }

func (s notConfiguredShim) IsPlatformAdmin(ctx context.Context, userID string) (bool, error) {
	isAdmin, err := s.inner.IsPlatformAdmin(ctx, userID)
	if err != nil && errors.Is(err, platformadmin.ErrNotConfigured) {
		return false, fmt.Errorf("%w: %w", idtenantscope.ErrAdminsNotConfigured, err)
	}
	return isAdmin, err
}

// principalOf reads what the auth middleware published.
func principalOf(c *gin.Context) idtenantscope.Principal {
	return idtenantscope.Principal{
		UserID:     principal(c),
		Credential: credentialOf(c),
		// KeyOrgID is deliberately not populated: KeyBindsOrganization is off,
		// so the shared resolver would ignore it, and supplying a value the
		// policy says is meaningless is how it gets trusted later by accident.
	}
}

// credentialOf maps this application's auth_method vocabulary onto the shared
// one.
//
// The default is CredentialUnspecified — the NARROW reading, never elevated —
// rather than "session". internal/middleware/auth.go sets auth_method on every
// path that also sets user_id, so an unrecognised value is not a live case; it
// is what a future auth path would hit before anybody remembered this function,
// and the safe answer there is the restrictive one.
func credentialOf(c *gin.Context) idtenantscope.Credential {
	v, ok := c.Get("auth_method")
	if !ok {
		return idtenantscope.CredentialUnspecified
	}
	method, ok := v.(string)
	if !ok {
		return idtenantscope.CredentialUnspecified
	}
	switch method {
	case "apikey":
		return idtenantscope.CredentialAPIKey
	case "jwt", "jwt_cookie":
		return idtenantscope.CredentialSession
	default:
		// "mtls" lands here, and correctly: internal/auth/mtls never publishes
		// a user_id, so there is no principal to elevate in the first place.
		return idtenantscope.CredentialUnspecified
	}
}

// principal returns the authenticated user id the auth middleware published.
func principal(c *gin.Context) string {
	v, ok := c.Get("user_id")
	if !ok {
		return ""
	}
	id, ok := v.(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(id)
}

// requestContext returns the request's context, or a background context for the
// synthetic gin.Context a test or a non-HTTP caller may hand in. A resolver that
// panicked on a nil Request would fail OPEN in the worst way: the request would
// never reach the code that decides whether it is permitted.
func requestContext(c *gin.Context) context.Context {
	if c.Request == nil {
		return context.Background()
	}
	return c.Request.Context()
}
