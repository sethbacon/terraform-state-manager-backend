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
)

// rwPairs encodes write-implies-read relationships: holding the write scope
// implicitly satisfies the paired read scope.
var rwPairs = idauth.ReadWritePairs{
	string(ScopeStateRead): string(ScopeStateWrite),
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
	}
}
