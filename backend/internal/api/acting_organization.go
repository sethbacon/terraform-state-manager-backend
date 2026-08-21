// acting_organization.go resolves the organization a WRITE belongs to, for the
// handlers that create a row on one of #393's nine partition roots.
//
// It is one function rather than a line in each handler because there are three
// ways to get this wrong and only one of them is obvious.
package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	idmodels "github.com/sethbacon/terraform-suite-identity/identity/models"
	idstore "github.com/sethbacon/terraform-suite-identity/identity/store"
	idtenantscope "github.com/sethbacon/terraform-suite-identity/identity/tenantscope"

	"github.com/terraform-state-manager/terraform-state-manager/internal/tenantscope"
)

// organizationExistence answers whether an organization id names a real
// organization.
//
// An interface rather than the concrete repository for the reason
// tenantscope's Memberships is one: the case that must be got right is the
// verifier FAILING, and a real repository can only be made to fail by taking its
// database away. A one-method seam makes that a table row.
//
// *idstore.OrganizationRepository satisfies it.
type organizationExistence interface {
	GetByID(ctx context.Context, id string, scope idstore.OrgScope) (*idmodels.Organization, error)
}

// actingOrganization returns the organization a write belongs to, or writes the
// response and returns "" when it cannot.
//
// # Three failures, deliberately distinguished
//
// NOT RESOLVED is a 500, not an empty scope. tenantscope.FromContext's second
// return value separates "resolved, and permits nothing" from "the route was
// never wired with middleware.TenantScope". Treating the second as the first
// would let a missing router line look like a caller with no memberships — the
// quietest possible way to reintroduce #393, because the request still gets a
// sensible-looking 403 and nothing says the scope was never computed.
//
// NO CHOICE MADE is a 400 naming the header. A caller who reaches several
// organizations and named none is not an error condition, it is an unfinished
// request: the picker exists precisely so they can say. Returning 403 would tell
// them they lack authority they actually hold.
//
// NAMED AN ORGANIZATION THEY CANNOT REACH is a 403, and the message deliberately
// does not say whether it exists. The shared resolver refuses without echoing
// the id back or listing what the caller could have chosen; repeating either
// here would rebuild the oracle it avoids.
//
// # And one that is not about the caller at all
//
// THE ORGANIZATION MUST EXIST. Scope.Permits returns true for ANY id when the
// caller is a platform administrator — the shared package documents this and
// delegates the check to the application, because "does this organization
// exist" is a question only the application knows how to ask (the partition
// deliberately carries no foreign key into identity, which may be a different
// database).
//
// Skipping it is reachable, not theoretical: a platform admin sending any uuid
// at all would stamp a row into an organization that names nothing. Under Phase
// 4's NOT NULL that row is WELL-FORMED and, per the organization predicate,
// invisible to every tenant while remaining visible to platform admins — an
// orphaned row nobody can be given and no tenant can be shown.
func actingOrganization(c *gin.Context, orgs organizationExistence) string {
	scope, resolved := tenantscope.FromContext(c)
	if !resolved {
		serverError(c, errNoTenantScope,
			"the tenant scope was not resolved for this route")
		return ""
	}

	orgID, err := tenantscope.ActingOrganization(c, scope)
	switch {
	case errors.Is(err, idtenantscope.ErrAmbiguousActingOrganization):
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "name the organization to act in via the " +
				idtenantscope.ActingOrganizationHeader + " header",
		})
		return ""
	case errors.Is(err, idtenantscope.ErrNoActingOrganization):
		c.JSON(http.StatusForbidden, gin.H{
			"error": "you do not belong to an organization that may create this",
		})
		return ""
	case errors.Is(err, idtenantscope.ErrActingOrganizationNotPermitted):
		c.JSON(http.StatusForbidden, gin.H{
			"error": "you may not act in that organization",
		})
		return ""
	case err != nil:
		serverError(c, err, "failed to resolve the acting organization")
		return ""
	}

	// THE EXISTENCE CHECK IS ONLY FOR A PLATFORM ADMINISTRATOR, and that is not
	// an optimisation.
	//
	// For an ordinary caller the id has already passed Scope.Permits, which means
	// it came out of their own memberships — and there are no memberships of an
	// organization that does not exist. Looking it up again would be redundant,
	// and it would put a platform-wide identity read on the ordinary write path,
	// which this repository's own guard test rightly objects to.
	//
	// A platform administrator is the case where Permits answers true for ANY id,
	// so nothing has confirmed the organization is real. There is no tenant to
	// narrow that lookup to — the caller reaches every organization by
	// definition — which is exactly why it is registered as a reviewed
	// platform-wide site rather than scoped.
	if !scope.PlatformAdmin {
		return orgID
	}
	if orgs == nil {
		// No verifier wired. Refusing is the only safe answer: the alternative
		// is stamping an id nothing confirmed, which is the orphan case above.
		serverError(c, errNoOrganizationVerifier,
			"cannot verify the acting organization")
		return ""
	}
	org, err := orgs.GetByID(c.Request.Context(), orgID, idstore.OrgScopeAllOrganizations())
	if err != nil || org == nil {
		// Same message as "not permitted", deliberately. A caller who can tell
		// "no such organization" from "not yours" can enumerate the deployment's
		// organizations with a uuid generator.
		c.JSON(http.StatusForbidden, gin.H{
			"error": "you may not act in that organization",
		})
		return ""
	}
	return orgID
}

var (
	errNoTenantScope          = errors.New("api: middleware.TenantScope is not registered on this route")
	errNoOrganizationVerifier = errors.New("api: no organization repository is wired to verify the acting organization")
)

// Compile-time proof that the real repository satisfies the seam. If identity's
// signature moves, this file stops building rather than the interface quietly
// being satisfied by nothing.
var _ organizationExistence = (*idstore.OrganizationRepository)(nil)
