package notify

import "testing"

// ForOrganization is the single conversion from a TSM organization id to a
// channel query scope, and the case that matters is the empty one.
//
// The rows that raise alerts -- drift_records, drift_runs, health_runs -- all
// reference their parent ON DELETE SET NULL, so a run whose source was deleted
// survives with no organization. If that produced "no scope", the fan-out would
// select every enabled channel in the deployment and announce one tenant's
// stuck run to every other tenant's webhook. Failing OPEN here is the original
// bug, restored through the thing meant to fix it.

func TestForOrganization_EmptyReachesNobody(t *testing.T) {
	scope := orgScopeFor("")
	// MatchesNothing is false for an all-organizations scope AND for one naming
	// any organization, so this single assertion rules out both the leak and a
	// mistaken narrow answer.
	if !scope.MatchesNothing() {
		t.Errorf("an empty organization produced scope %+v, which matches something. An event "+
			"with no organization -- a run whose source was deleted -- would then be delivered "+
			"to channels it does not belong to.", scope)
	}
}

func TestForOrganization_NamesOnlyThatOrganization(t *testing.T) {
	const org = "aaaaaaaa-0000-4000-8000-000000000001"
	scope := orgScopeFor(org)
	if scope.MatchesNothing() {
		t.Fatal("a named organization produced a scope that matches nothing: its own alerts " +
			"would be silently dropped")
	}
	if len(scope.OrganizationIDs()) != 1 || scope.OrganizationIDs()[0] != org {
		t.Errorf("scope organizations = %v, want exactly [%s]", scope.OrganizationIDs(), org)
	}
}
