// group_mapping_guard.go guards reconcileManagedMemberships against an
// IdP-driven group mapping resolving to a role_template that carries
// auth.ScopeAdmin, the grant-all wildcard scope.
package api

import (
	"context"
	"errors"
	"fmt"

	idstore "github.com/sethbacon/terraform-suite-identity/identity/store"

	"github.com/terraform-state-manager/terraform-state-manager/internal/auth"
)

// guardProvisionableRole rejects a group mapping's resolved role_template when
// its scopes carry auth.ScopeAdmin ("admin"). Call this in
// reconcileManagedMemberships immediately before trusting a mapped role name
// for an automatic, IdP-driven membership write (AddMemberWithParams /
// UpdateMemberRole) — never on a role read back for an already-trusted, direct
// admin action (e.g. assignRole's default_role fallback, which is a static,
// admin-configured value, not IdP-driven, and must not be affected by this
// guard).
//
// This is defense-in-depth, not a fix for an active exploit: the group-mapping
// CONFIG that names a Role is itself only reachable by a caller who already
// holds ScopeAdmin (the SSO settings/config surface sits behind the admin
// scope gate), so an unprivileged actor cannot plant Role: "admin" in a
// mapping today. But nothing in reconcileManagedMemberships itself refuses to
// auto-apply a role_template carrying ScopeAdmin once a mapping names one —
// this guards against that changing in the future (e.g. a lower-privileged,
// org-scoped mapping-writer API, or a SCIM-driven mapping writer — this app
// already has a SCIM surface), per terraform-suite-identity's
// ValidateProvisionableScopes doc and this repo's issue #173.
//
// A role_template name that does not resolve to a row returns nil (no error):
// the caller's own AddMemberWithParams/UpdateMemberRole performs the
// authoritative name lookup immediately afterward and surfaces its own clear
// error there, so this guard does not need to duplicate that failure mode.
// Since terraform-suite-identity v0.24.0 the store reports "no such template"
// as an error wrapping store.ErrNotFound instead of (nil, nil), so that
// contract is now kept by matching the sentinel — the `rt == nil` branch below
// no longer fires, and without this the guard would reject every mapping whose
// role name does not exist rather than deferring to the write's own error.
//
// Any other lookup failure is returned (fails closed) — a transient DB error
// here should not silently let an unverified role's scopes through.
func (h *AuthHandlers) guardProvisionableRole(ctx context.Context, roleTemplateName string) error {
	rt, err := h.roleRepo.GetRoleTemplateByName(ctx, roleTemplateName)
	switch {
	case errors.Is(err, idstore.ErrNotFound):
		return nil // unresolved name: the subsequent membership write reports it
	case err != nil:
		return fmt.Errorf("look up role template %q: %w", roleTemplateName, err)
	case rt == nil:
		return nil
	}
	return auth.ValidateProvisionableScopes(rt.Scopes)
}
