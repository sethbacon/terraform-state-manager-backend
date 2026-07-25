package auth

import idauth "github.com/sethbacon/terraform-suite-identity/identity/auth"

// Scope is a permission string of the form "<domain>:<action>".
type Scope string

// Application scopes. ScopeAdmin is the wildcard recognised by the identity
// scope checker (grants every scope).
const (
	ScopeStateRead     Scope = "state:read"
	ScopeStateWrite    Scope = "state:write"
	ScopeStateDrift    Scope = "state:drift"
	ScopeStateExecute  Scope = "state:execute"
	ScopeStateTransfer Scope = "state:transfer"
	ScopeSourcesManage Scope = "sources:manage"
	ScopeSCIMProvision Scope = "scim:provision"
	ScopeAdmin         Scope = "admin"

	// Organization-management scopes (identity-core, re-exported). org_owner
	// (see AppRoleTemplates) carries organizations:write, so a caller can
	// administer the org they belong to without the flat admin wildcard;
	// org_provisioner carries organizations:create/:read to stand up new
	// organizations without any other admin rights.
	ScopeOrganizationsRead   Scope = idauth.ScopeOrganizationsRead
	ScopeOrganizationsWrite  Scope = idauth.ScopeOrganizationsWrite
	ScopeOrganizationsCreate Scope = idauth.ScopeOrganizationsCreate
	ScopeUsersRead           Scope = idauth.ScopeUsersRead
	ScopeAPIKeysManage       Scope = idauth.ScopeAPIKeysManage
)

// rwPairs encodes write-implies-read relationships: holding the write scope
// implicitly satisfies the paired read scope. organizations:create is
// deliberately NOT paired here — it is a standalone provisioning right, not
// implied by organizations:write, so org_owner never gains the ability to
// create brand-new organizations just by managing its own.
var rwPairs = idauth.ReadWritePairs{
	string(ScopeStateRead):         string(ScopeStateWrite),
	string(ScopeOrganizationsRead): string(ScopeOrganizationsWrite),
}

// HasScope reports whether userScopes satisfies the required scope.
func HasScope(userScopes []string, s Scope) bool {
	return idauth.HasScope(userScopes, string(s), rwPairs)
}

// HasAnyScope reports whether userScopes satisfies at least one required scope.
func HasAnyScope(userScopes []string, scopes []Scope) bool {
	return idauth.HasAnyScope(userScopes, toStrings(scopes), rwPairs)
}

// HasAllScopes reports whether userScopes satisfies every required scope.
func HasAllScopes(userScopes []string, scopes []Scope) bool {
	return idauth.HasAllScopes(userScopes, toStrings(scopes), rwPairs)
}

func toStrings(scopes []Scope) []string {
	out := make([]string, len(scopes))
	for i, s := range scopes {
		out[i] = string(s)
	}
	return out
}

// ValidateProvisionableScopes rejects ScopeAdmin ("admin") from a scope list,
// naming it specifically, and returns nil otherwise. Call this when mapping
// externally-influenced data (an OIDC/SAML/LDAP IdP group claim, or any other
// value a lower-trust source contributes) onto a role_template's scopes, BEFORE
// that resolved scope list is trusted or persisted via an automatic, IdP-driven
// membership write — never on scopes read back from an already-trusted,
// admin-seeded role_template used in a direct admin action, where carrying
// ScopeAdmin is expected and legitimate.
//
// Thin wrapper over idauth.ValidateProvisionableScopes, matching this file's
// existing pattern of re-exporting the shared identity module's scope-checking
// helpers.
func ValidateProvisionableScopes(scopes []string) error {
	return idauth.ValidateProvisionableScopes(scopes)
}

// RoleTemplateSeed defines an app-owned role→scope mapping. The application owns
// only the role templates in the shared identity schema; these are upserted at
// startup (see internal/bootstrap).
type RoleTemplateSeed struct {
	Name        string
	DisplayName string
	Description string
	Scopes      []string
}

// AppRoleTemplates returns the role templates owned by this application.
func AppRoleTemplates() []RoleTemplateSeed {
	return []RoleTemplateSeed{
		{
			Name:        "admin",
			DisplayName: "Administrator",
			Description: "Full access to all features.",
			Scopes:      []string{string(ScopeAdmin)},
		},
		{
			Name:        "editor",
			DisplayName: "Editor",
			Description: "Read, edit, and transfer state; manage sources; run drift and version checks.",
			Scopes: []string{
				string(ScopeStateRead), string(ScopeStateWrite), string(ScopeStateTransfer),
				string(ScopeStateDrift), string(ScopeStateExecute), string(ScopeSourcesManage),
			},
		},
		{
			Name:        "operator",
			DisplayName: "Operator",
			Description: "Read state and run drift and version/health checks.",
			Scopes: []string{
				string(ScopeStateRead), string(ScopeStateDrift), string(ScopeStateExecute),
			},
		},
		{
			Name:        "viewer",
			DisplayName: "Viewer",
			Description: "Read-only access to state and analysis.",
			Scopes:      []string{string(ScopeStateRead)},
		},
		{
			Name:        "org_owner",
			DisplayName: "Organization Owner",
			Description: "Manage an organization's membership and API keys; full state operations across the shared global state pool (state sources are not partitioned per organization).",
			Scopes: []string{
				string(ScopeOrganizationsWrite), string(ScopeUsersRead), string(ScopeAPIKeysManage),
				string(ScopeStateRead), string(ScopeStateWrite), string(ScopeStateDrift),
				string(ScopeStateExecute), string(ScopeStateTransfer), string(ScopeSourcesManage),
			},
		},
		{
			Name:        "org_provisioner",
			DisplayName: "Organization Provisioner",
			Description: "Create new organizations and view the organization directory.",
			Scopes:      []string{string(ScopeOrganizationsCreate), string(ScopeOrganizationsRead)},
		},
	}
}
