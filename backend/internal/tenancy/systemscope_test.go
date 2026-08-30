package tenancy

import (
	"errors"
	"strings"
	"testing"
)

// The constructor is the WHOLE surface: nothing else in the codebase may
// assemble a system authority by hand, so these pin what it refuses and what
// the value it produces permits.

func TestSystemActingIn_RefusesAnUnownedRow(t *testing.T) {
	_, err := SystemActingIn("", "schedules", "sc1")
	if !errors.Is(err, ErrSystemScopeUnowned) {
		t.Fatalf("deriving from a row with no organization returned %v; want ErrSystemScopeUnowned. "+
			"A row that belongs to no organization confers authority over none — defaulting here "+
			"is the privileged-worker option #393 rejected.", err)
	}
	if !strings.Contains(err.Error(), "schedules/sc1") {
		t.Errorf("the refusal %q does not name the row it derives from; an operator cannot find "+
			"the poisoned row from a message that withholds its coordinates", err.Error())
	}
}

func TestSystemActingIn_RefusesAnonymousProvenance(t *testing.T) {
	for _, tc := range []struct{ table, id string }{
		{"", "sc1"},
		{"schedules", ""},
		{"  ", "  "},
	} {
		if _, err := SystemActingIn("11111111-1111-4111-8111-111111111111", tc.table, tc.id); err == nil {
			t.Errorf("SystemActingIn(org, %q, %q) succeeded; a system scope with no row to trace "+
				"back to is ambient authority, which is the model option B rejected", tc.table, tc.id)
		}
	}
}

func TestSystemActingIn_DerivesExactlyOneOrganization(t *testing.T) {
	const org = "11111111-1111-4111-8111-111111111111"
	s, err := SystemActingIn(" "+org+" ", "schedules", "sc1")
	if err != nil {
		t.Fatalf("SystemActingIn: %v", err)
	}
	scope := s.Scope()
	if scope.PlatformAdmin {
		t.Fatal("a system scope resolved as PLATFORM ADMIN. That is the privileged worker #393 " +
			"exists to eliminate: it would reach every organization's rows, including NULL-stamped ones.")
	}
	if len(scope.OrgIDs) != 1 || scope.OrgIDs[0] != org {
		t.Fatalf("Scope().OrgIDs = %v, want exactly [%s]", scope.OrgIDs, org)
	}
	if !scope.Permits(org) {
		t.Error("the derived scope does not permit its own organization")
	}
	if scope.Permits("22222222-2222-4222-8222-222222222222") {
		t.Error("the derived scope permits an organization the owning row never named")
	}
	if s.OrganizationID() != org {
		t.Errorf("OrganizationID() = %q, want %q", s.OrganizationID(), org)
	}
}

// TestSystemScope_ProvenanceIsDistinguishable is the auditable-provenance
// property the #393 decision requires: wherever a system-derived authority is
// consumed or logged, it must be tellable from a request-resolved one.
//
// Two mechanisms, both asserted. The TYPE is distinct — a request path resolves
// a tenantscope.Scope and can never produce a SystemScope, so a seam typed
// SystemScope cannot be handed request authority by accident. And the ORIGIN
// string carries a reserved prefix naming the row, so a log line or refusal
// built from it is attributable without the reader knowing the type system.
func TestSystemScope_ProvenanceIsDistinguishable(t *testing.T) {
	s, err := SystemActingIn("11111111-1111-4111-8111-111111111111", "schedules", "sc1")
	if err != nil {
		t.Fatalf("SystemActingIn: %v", err)
	}
	if got := s.Origin(); got != "system:schedules/sc1" {
		t.Fatalf("Origin() = %q, want %q", got, "system:schedules/sc1")
	}
	if !strings.HasPrefix(s.Origin(), "system:") {
		t.Error("Origin() does not carry the system: prefix that separates it from request provenance")
	}
	if !strings.Contains(s.String(), "schedules/sc1") ||
		!strings.Contains(s.String(), "11111111-1111-4111-8111-111111111111") {
		t.Errorf("String() = %q; a log line built from it cannot name the row and organization", s.String())
	}
}

func TestSystemScope_ZeroValuePermitsNothing(t *testing.T) {
	var s SystemScope
	if !s.IsZero() {
		t.Fatal("the zero SystemScope does not report IsZero")
	}
	if got := s.Scope(); got.PlatformAdmin || len(got.OrgIDs) != 0 || !got.Empty() {
		t.Fatalf("the zero SystemScope yields %+v; it must yield the zero Scope, which permits nothing", got)
	}
	if s.Origin() != "" {
		t.Errorf("the zero SystemScope claims provenance %q", s.Origin())
	}
}
