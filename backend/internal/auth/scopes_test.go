package auth

import "testing"

func TestHasScope(t *testing.T) {
	tests := []struct {
		name       string
		userScopes []string
		required   Scope
		want       bool
	}{
		// Exact match
		{"exact match state:read", []string{"state:read"}, ScopeStateRead, true},
		{"exact match admin", []string{"admin"}, ScopeAdmin, true},
		// Admin wildcard grants everything
		{"admin grants state:read", []string{"admin"}, ScopeStateRead, true},
		{"admin grants state:write", []string{"admin"}, ScopeStateWrite, true},
		{"admin grants sources:manage", []string{"admin"}, ScopeSourcesManage, true},
		{"admin grants scim:provision", []string{"admin"}, ScopeSCIMProvision, true},
		// Write implies read
		{"state:write implies state:read", []string{"state:write"}, ScopeStateRead, true},
		// Write does NOT imply unrelated scopes
		{"state:write does not imply state:transfer", []string{"state:write"}, ScopeStateTransfer, false},
		{"state:write does not imply sources:manage", []string{"state:write"}, ScopeSourcesManage, false},
		// No match
		{"no scopes", []string{}, ScopeStateRead, false},
		{"wrong scope", []string{"state:drift"}, ScopeStateTransfer, false},
		{"read does not imply write", []string{"state:read"}, ScopeStateWrite, false},
		{"non-admin does not grant admin", []string{"state:read", "state:write"}, ScopeAdmin, false},
		// Multiple scopes, one matches
		{"one of many matches", []string{"state:drift", "state:execute"}, ScopeStateExecute, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := HasScope(tt.userScopes, tt.required)
			if got != tt.want {
				t.Errorf("HasScope(%v, %q) = %v, want %v", tt.userScopes, tt.required, got, tt.want)
			}
		})
	}
}

func TestHasAnyScope(t *testing.T) {
	tests := []struct {
		name           string
		userScopes     []string
		requiredScopes []Scope
		want           bool
	}{
		{"matches first", []string{"state:read"}, []Scope{ScopeStateRead, ScopeStateDrift}, true},
		{"matches second", []string{"state:drift"}, []Scope{ScopeStateRead, ScopeStateDrift}, true},
		{"matches none", []string{"state:execute"}, []Scope{ScopeStateRead, ScopeStateDrift}, false},
		{"empty required", []string{"state:read"}, []Scope{}, false},
		{"empty user scopes", []string{}, []Scope{ScopeStateRead}, false},
		{"admin matches any", []string{"admin"}, []Scope{ScopeStateTransfer, ScopeSourcesManage}, true},
		{"write satisfies read requirement", []string{"state:write"}, []Scope{ScopeStateRead}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := HasAnyScope(tt.userScopes, tt.requiredScopes)
			if got != tt.want {
				t.Errorf("HasAnyScope(%v, %v) = %v, want %v", tt.userScopes, tt.requiredScopes, got, tt.want)
			}
		})
	}
}

func TestHasAllScopes(t *testing.T) {
	tests := []struct {
		name           string
		userScopes     []string
		requiredScopes []Scope
		want           bool
	}{
		{"has all", []string{"state:read", "state:drift"}, []Scope{ScopeStateRead, ScopeStateDrift}, true},
		{"missing one", []string{"state:read"}, []Scope{ScopeStateRead, ScopeStateDrift}, false},
		// The identity module is fail-closed: an empty required list denies.
		{"empty required denies", []string{"state:read"}, []Scope{}, false},
		{"empty user no requirements denies", []string{}, []Scope{}, false},
		{"empty user has requirements", []string{}, []Scope{ScopeStateRead}, false},
		{"admin has all", []string{"admin"}, []Scope{ScopeStateRead, ScopeStateWrite, ScopeSourcesManage}, true},
		{"write covers read in a set", []string{"state:write", "state:drift"}, []Scope{ScopeStateRead, ScopeStateDrift}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := HasAllScopes(tt.userScopes, tt.requiredScopes)
			if got != tt.want {
				t.Errorf("HasAllScopes(%v, %v) = %v, want %v", tt.userScopes, tt.requiredScopes, got, tt.want)
			}
		})
	}
}

func TestValidateProvisionableScopes(t *testing.T) {
	tests := []struct {
		name    string
		scopes  []string
		wantErr bool
	}{
		{"empty list", []string{}, false},
		{"non-admin scopes", []string{"state:read", "sources:manage"}, false},
		{"admin alone", []string{"admin"}, true},
		{"admin among others", []string{"state:read", "admin"}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateProvisionableScopes(tt.scopes)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateProvisionableScopes(%v) error = %v, wantErr %v", tt.scopes, err, tt.wantErr)
			}
		})
	}
}

func TestAppRoleTemplates(t *testing.T) {
	templates := AppRoleTemplates()
	if len(templates) != 6 {
		t.Fatalf("expected 6 role templates, got %d", len(templates))
	}

	defined := map[string]bool{
		string(ScopeStateRead): true, string(ScopeStateWrite): true,
		string(ScopeStateDrift): true, string(ScopeStateExecute): true,
		string(ScopeStateTransfer): true, string(ScopeSourcesManage): true,
		string(ScopeSCIMProvision): true, string(ScopeAdmin): true,
		string(ScopeOrganizationsRead): true, string(ScopeOrganizationsWrite): true,
		string(ScopeOrganizationsCreate): true, string(ScopeUsersRead): true,
		string(ScopeAPIKeysManage): true,
	}

	seen := map[string]bool{}
	byName := map[string]RoleTemplateSeed{}
	for _, tpl := range templates {
		if seen[tpl.Name] {
			t.Errorf("duplicate role template name %q", tpl.Name)
		}
		seen[tpl.Name] = true
		byName[tpl.Name] = tpl

		if tpl.DisplayName == "" || tpl.Description == "" {
			t.Errorf("role %q has empty display name or description", tpl.Name)
		}
		if len(tpl.Scopes) == 0 {
			t.Errorf("role %q has no scopes", tpl.Name)
		}
		for _, s := range tpl.Scopes {
			if !defined[s] {
				t.Errorf("role %q references undefined scope %q", tpl.Name, s)
			}
		}
	}

	for _, want := range []string{"admin", "editor", "operator", "viewer", "org_owner", "org_provisioner"} {
		if !seen[want] {
			t.Errorf("missing expected role template %q", want)
		}
	}

	if admin := byName["admin"]; len(admin.Scopes) != 1 || admin.Scopes[0] != string(ScopeAdmin) {
		t.Errorf("admin role scopes = %v, want [admin]", admin.Scopes)
	}
	if viewer := byName["viewer"]; len(viewer.Scopes) != 1 || viewer.Scopes[0] != string(ScopeStateRead) {
		t.Errorf("viewer role scopes = %v, want [state:read]", viewer.Scopes)
	}
}

// TestAppRoleTemplates_OrgOwnerHasNoEscalationScopes is the app-level ceiling
// invariant for the org-owner parity fix: org_owner must never carry the flat
// admin wildcard, users:write, or organizations:create — otherwise an
// org-scoped role could self-escalate to platform-admin or provision brand-new
// organizations, defeating the point of moving org management off the admin
// wildcard. org_provisioner is checked separately: it must carry EXACTLY
// organizations:create + organizations:read and nothing else (no state/user
// access at all).
func TestAppRoleTemplates_OrgOwnerHasNoEscalationScopes(t *testing.T) {
	templates := AppRoleTemplates()
	byName := map[string]RoleTemplateSeed{}
	for _, tpl := range templates {
		byName[tpl.Name] = tpl
	}

	owner, ok := byName["org_owner"]
	if !ok {
		t.Fatal("org_owner role template not found")
	}
	forbidden := []string{string(ScopeAdmin), "users:write", string(ScopeOrganizationsCreate)}
	for _, f := range forbidden {
		for _, s := range owner.Scopes {
			if s == f {
				t.Errorf("org_owner scopes = %v, must never include %q", owner.Scopes, f)
			}
		}
	}
	if !HasScope(owner.Scopes, ScopeOrganizationsWrite) {
		t.Errorf("org_owner scopes = %v, want organizations:write present", owner.Scopes)
	}

	provisioner, ok := byName["org_provisioner"]
	if !ok {
		t.Fatal("org_provisioner role template not found")
	}
	wantProvisioner := []string{string(ScopeOrganizationsCreate), string(ScopeOrganizationsRead)}
	if len(provisioner.Scopes) != len(wantProvisioner) {
		t.Fatalf("org_provisioner scopes = %v, want exactly %v", provisioner.Scopes, wantProvisioner)
	}
	for i, want := range wantProvisioner {
		if provisioner.Scopes[i] != want {
			t.Errorf("org_provisioner scopes = %v, want exactly %v", provisioner.Scopes, wantProvisioner)
			break
		}
	}
}
