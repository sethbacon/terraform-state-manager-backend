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
			name:        "last matching mapping wins for the same org",
			groups:      []string{"tf-admins", "tf-viewers"},
			wantDesired: map[string]string{"acme": "viewer", "network": ""},
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
			desired, managed := resolveGroupMappings(tc.groups, mappings)

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
	desired, managed := resolveGroupMappings([]string{"anything"}, nil)
	if len(desired) != 0 || len(managed) != 0 {
		t.Fatalf("expected empty maps for no mappings, got desired=%v managed=%v", desired, managed)
	}
}
