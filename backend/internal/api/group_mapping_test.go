package api

import (
	"reflect"
	"testing"

	"github.com/terraform-state-manager/terraform-state-manager/internal/config"
)

func TestResolveGroupMappings(t *testing.T) {
	mappings := []config.OIDCGroupMapping{
		{Group: "tf-admins", Organization: "acme", Role: "admin"},
		{Group: "tf-viewers", Organization: "acme", Role: "viewer"},
		{Group: "net-team", Organization: "network", Role: "editor"},
	}

	cases := []struct {
		name        string
		groups      []string
		wantDesired map[string]string
		wantManaged []string
	}{
		{
			name:        "matched admin group",
			groups:      []string{"tf-admins"},
			wantDesired: map[string]string{"acme": "admin"},
			wantManaged: []string{"acme", "network"},
		},
		{
			name:        "no matching group — desired empty, managed orgs still listed (to be revoked)",
			groups:      []string{"unrelated-group"},
			wantDesired: map[string]string{},
			wantManaged: []string{"acme", "network"},
		},
		{
			name:        "forged/extra groups not in any mapping are ignored",
			groups:      []string{"admin", "superuser", "*", "acme:admin"},
			wantDesired: map[string]string{},
			wantManaged: []string{"acme", "network"},
		},
		{
			name:        "multiple orgs matched",
			groups:      []string{"tf-admins", "net-team"},
			wantDesired: map[string]string{"acme": "admin", "network": "editor"},
			wantManaged: []string{"acme", "network"},
		},
		{
			// FIRST matching mapping wins, since #488. This case asserted
			// last-wins until the estate settled on one rule across both
			// applications (identity#269): the registry took the first match and
			// this app took the last, off the same shared type, so one stored
			// list granted different roles depending on which app read it.
			//
			// First-wins was chosen because appending a mapping cannot then
			// change the outcome for anyone already matched — what an
			// authorization list edited incrementally through a UI needs.
			name:        "first matching mapping wins for the same org",
			groups:      []string{"tf-admins", "tf-viewers"},
			wantDesired: map[string]string{"acme": "admin", "network": ""},
			wantManaged: []string{"acme", "network"},
		},
		{
			name:        "empty groups — nothing desired, all mapping orgs managed",
			groups:      nil,
			wantDesired: map[string]string{},
			wantManaged: []string{"acme", "network"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			desired, managed, _ := resolveGroupMappings(tc.groups, mappings)

			// "last mapping wins" case expresses network as "" meaning absent.
			want := map[string]string{}
			for k, v := range tc.wantDesired {
				if v != "" {
					want[k] = v
				}
			}
			if !reflect.DeepEqual(desired, want) {
				t.Errorf("desired = %v, want %v", desired, want)
			}
			for _, org := range tc.wantManaged {
				if _, ok := managed[org]; !ok {
					t.Errorf("expected %q in managed set %v", org, managed)
				}
			}
			if len(managed) != len(tc.wantManaged) {
				t.Errorf("managed set size = %d, want %d (%v)", len(managed), len(tc.wantManaged), managed)
			}
		})
	}
}

func TestResolveGroupMappings_Empty(t *testing.T) {
	desired, managed, _ := resolveGroupMappings([]string{"anything"}, nil)
	if len(desired) != 0 || len(managed) != 0 {
		t.Fatalf("expected empty maps for no mappings, got desired=%v managed=%v", desired, managed)
	}
}

// #488 — admin preservation must not depend on which mapping wins.
//
// The mechanism it protects: a mapping resolving to a role carrying ScopeAdmin
// is refused by guardProvisionableRole, which deliberately does NOT fall
// through to the revoke branch — so a matching-but-refused admin mapping is the
// only supported way to hold a manually-granted admin membership in an
// IdP-managed organization.
//
// Under last-wins that worked only while the admin mapping was LAST. Flipping
// the estate to first-wins would have let a weaker mapping win, PASS the guard,
// and demote a real administrator with no error anywhere. These pin the
// property that replaced the ordering dependence.

func TestResolveGroupMappings_AllMatchingReportsEveryMatchedRole(t *testing.T) {
	mappings := []config.OIDCGroupMapping{
		{Group: "tf-editors", Organization: "acme", Role: "editor"},
		{Group: "tf-admins", Organization: "acme", Role: "admin"},
	}
	_, _, all := resolveGroupMappings([]string{"tf-editors", "tf-admins"}, mappings)

	got := all["acme"]
	if len(got) != 2 {
		t.Fatalf("allMatching[acme] = %v, want both matched roles regardless of which won", got)
	}
	var seenAdmin bool
	for _, r := range got {
		if r == "admin" {
			seenAdmin = true
		}
	}
	if !seenAdmin {
		t.Error("the refused role must be reported even though it did not win; that is the whole point")
	}
}

// The ordering-independence itself: the SAME mapping set in either order must
// report the same matched-role set, so the guard's answer cannot change with it.
func TestResolveGroupMappings_AllMatchingIsOrderIndependent(t *testing.T) {
	adminLast := []config.OIDCGroupMapping{
		{Group: "tf-editors", Organization: "acme", Role: "editor"},
		{Group: "tf-admins", Organization: "acme", Role: "admin"},
	}
	adminFirst := []config.OIDCGroupMapping{
		{Group: "tf-admins", Organization: "acme", Role: "admin"},
		{Group: "tf-editors", Organization: "acme", Role: "editor"},
	}
	groups := []string{"tf-editors", "tf-admins"}

	_, _, a := resolveGroupMappings(groups, adminLast)
	_, _, b := resolveGroupMappings(groups, adminFirst)

	contains := func(rs []string, want string) bool {
		for _, r := range rs {
			if r == want {
				return true
			}
		}
		return false
	}
	if !contains(a["acme"], "admin") || !contains(b["acme"], "admin") {
		t.Errorf("the admin role must appear in both orderings: last=%v first=%v", a["acme"], b["acme"])
	}
	// The WINNER legitimately differs between the two orderings under
	// first-wins. That is exactly why the guard must not read the winner.
	da, _, _ := resolveGroupMappings(groups, adminLast)
	db, _, _ := resolveGroupMappings(groups, adminFirst)
	if da["acme"] == db["acme"] {
		t.Fatalf("this test is inert: the winner should differ between orderings, both were %q", da["acme"])
	}
}
