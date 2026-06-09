package ldap

import (
	"reflect"
	"testing"

	"github.com/terraform-state-manager/terraform-state-manager/internal/config"
)

func baseCfg() config.LDAPConfig {
	return config.LDAPConfig{
		Enabled: true, Host: "ldap.example.com", BaseDN: "dc=example,dc=com",
		BindDN: "cn=svc,dc=example,dc=com", UserFilter: "(uid=%s)",
	}
}

func TestNewProvider_Validation(t *testing.T) {
	cases := map[string]func(*config.LDAPConfig){
		"disabled":            func(c *config.LDAPConfig) { c.Enabled = false },
		"no host":             func(c *config.LDAPConfig) { c.Host = "" },
		"no base_dn":          func(c *config.LDAPConfig) { c.BaseDN = "" },
		"no bind_dn":          func(c *config.LDAPConfig) { c.BindDN = "" },
		"no user_filter":      func(c *config.LDAPConfig) { c.UserFilter = "" },
		"filter without %s":   func(c *config.LDAPConfig) { c.UserFilter = "(uid=fixed)" },
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := baseCfg()
			mut(&cfg)
			if _, err := NewProvider(cfg); err == nil {
				t.Fatalf("expected validation error for %q", name)
			}
		})
	}
	if _, err := NewProvider(baseCfg()); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
}

// Authenticate must reject empty username/password before any bind, defeating
// LDAP unauthenticated-bind (no network required to reach this guard).
func TestAuthenticate_RejectsEmptyCredentials(t *testing.T) {
	p, err := NewProvider(baseCfg())
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	for _, tc := range []struct{ user, pass string }{
		{"", "x"}, {"   ", "x"}, {"alice", ""},
	} {
		if _, err := p.Authenticate(tc.user, tc.pass); err == nil {
			t.Errorf("expected rejection for user=%q pass=%q", tc.user, tc.pass)
		}
	}
}

func TestResolveLDAPGroupMappings(t *testing.T) {
	mappings := []config.LDAPGroupMapping{
		{GroupDN: "cn=tf-admins,ou=groups,dc=example,dc=com", Organization: "default", Role: "admin"},
		{GroupDN: "cn=net,ou=groups,dc=example,dc=com", Organization: "network", Role: "editor"},
	}

	t.Run("case-insensitive DN match", func(t *testing.T) {
		desired, managed := ResolveLDAPGroupMappings(
			[]string{"CN=TF-Admins,OU=Groups,DC=example,DC=com"}, mappings)
		if !reflect.DeepEqual(desired, map[string]string{"default": "admin"}) {
			t.Fatalf("desired = %v", desired)
		}
		if len(managed) != 2 {
			t.Fatalf("managed = %v, want 2 orgs", managed)
		}
	})

	t.Run("no matching group -> empty desired, orgs still managed (revoked)", func(t *testing.T) {
		desired, managed := ResolveLDAPGroupMappings([]string{"cn=unrelated,dc=example,dc=com"}, mappings)
		if len(desired) != 0 {
			t.Fatalf("desired should be empty, got %v", desired)
		}
		if _, ok := managed["default"]; !ok {
			t.Fatal("default org should be managed (eligible for revocation)")
		}
	})
}
