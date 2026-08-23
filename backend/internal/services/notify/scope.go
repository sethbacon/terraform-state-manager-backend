package notify

import (
	identitynotify "github.com/sethbacon/terraform-suite-identity/identity/notify"
	idstore "github.com/sethbacon/terraform-suite-identity/identity/store"
)

// ForOrganization is the ONE place a TSM organization id becomes a channel
// query scope, and it exists because that conversion is where a fan-out silently
// becomes deployment-wide.
//
// # An unowned event reaches nobody, not everybody
//
// The rows that raise alerts -- drift_records, drift_runs, health_runs -- all
// reference their parent ON DELETE SET NULL, so a run whose source was deleted
// survives with no organization. Passing "no scope" for such an event would
// select EVERY enabled channel in the deployment, which is precisely the leak
// this closes: one tenant's infrastructure drift POSTed to another tenant's
// webhook, at a destination that tenant controls.
//
// So an empty organization yields a scope that matches nothing, and the caller
// logs it. Losing an alert is a bug worth fixing; delivering it to the wrong
// tenant is a disclosure that cannot be taken back.
//
// # Why not a plain helper at each call site
//
// identity's tenantscope doc warns that "a conversion is where a PlatformAdmin
// flag gets dropped". The same hazard applies here in the other direction: three
// raisers each writing their own conversion is three chances to write the
// permissive one. There is one, and it is tested.
func ForOrganization(organizationID string) identitynotify.ChannelQueryOption {
	return identitynotify.WithOrgScope(orgScopeFor(organizationID))
}

// orgScopeFor is the decision, split out from the option so it can be asserted
// directly. A ChannelQueryOption is opaque, so a test of ForOrganization alone
// could only check that it returns non-nil -- which is exactly the shape of a
// test that passes while the scope inside it is wrong.
func orgScopeFor(organizationID string) idstore.OrgScope {
	if organizationID == "" {
		// OrgScopeOrganizations with no ids matches nothing -- the fail-closed
		// direction. NOT OrgScopeAllOrganizations(), and not the zero value with
		// `unowned` set: an event with no organization must reach nobody.
		return idstore.OrgScopeOrganizations()
	}
	return idstore.OrgScopeOrganizations(organizationID)
}
