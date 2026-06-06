package models

import "testing"

func scopesContain(scopes []string, want string) bool {
	for _, s := range scopes {
		if s == want {
			return true
		}
	}
	return false
}

func TestPredefinedRoleTemplates_Names(t *testing.T) {
	templates := PredefinedRoleTemplates()
	if len(templates) != 4 {
		t.Fatalf("expected 4 role templates, got %d", len(templates))
	}

	want := map[string]bool{"admin": true, "analyst": true, "viewer": true, "operator": true}
	for _, tmpl := range templates {
		if !want[tmpl.Name] {
			t.Errorf("unexpected role template %q", tmpl.Name)
		}
		delete(want, tmpl.Name)

		if !tmpl.IsSystem {
			t.Errorf("role template %q should be a system template", tmpl.Name)
		}
		if tmpl.DisplayName == "" {
			t.Errorf("role template %q has an empty display name", tmpl.Name)
		}
		if tmpl.Description == nil || *tmpl.Description == "" {
			t.Errorf("role template %q has an empty description", tmpl.Name)
		}
		if len(tmpl.Scopes) == 0 {
			t.Errorf("role template %q has no scopes", tmpl.Name)
		}
	}
	if len(want) != 0 {
		t.Errorf("missing role templates: %v", want)
	}
}

// TestPredefinedRoleTemplates_Scopes locks the per-role scope invariants that
// give public<->identity parity under the cutover (these mirror migration 000001).
func TestPredefinedRoleTemplates_Scopes(t *testing.T) {
	byName := make(map[string][]string)
	for _, tmpl := range PredefinedRoleTemplates() {
		byName[tmpl.Name] = tmpl.Scopes
	}

	// admin carries the wildcard.
	if !scopesContain(byName["admin"], "admin") {
		t.Error("admin role template must include the 'admin' wildcard scope")
	}

	// analyst can run analysis but is not an administrator.
	if !scopesContain(byName["analyst"], "analysis:write") {
		t.Error("analyst should have analysis:write")
	}
	if scopesContain(byName["analyst"], "users:write") {
		t.Error("analyst must not have users:write")
	}

	// viewer is read-only.
	if !scopesContain(byName["viewer"], "analysis:read") {
		t.Error("viewer should have analysis:read")
	}
	if scopesContain(byName["viewer"], "analysis:write") {
		t.Error("viewer must not have analysis:write")
	}

	// operator has operational write access but not user/org administration.
	if !scopesContain(byName["operator"], "sources:write") {
		t.Error("operator should have sources:write")
	}
	if !scopesContain(byName["operator"], "users:read") {
		t.Error("operator should have users:read")
	}
	if scopesContain(byName["operator"], "users:write") {
		t.Error("operator must not have users:write")
	}
}
