package ldap

import "testing"

// TestNewProvider_DefaultsAndOverrides covers the remaining NewProvider
// branches: the implicit-TLS default port (636), an explicitly-set port, and
// non-empty attribute overrides (which skip the defaulting assignments).
func TestNewProvider_DefaultsAndOverrides(t *testing.T) {
	// UseTLS with no explicit port => defaults to 636; custom attrs skip defaulting.
	cfg := baseCfg()
	cfg.UseTLS = true
	cfg.UserAttrEmail = "email"
	cfg.UserAttrName = "cn"
	cfg.GroupMemberAttr = "memberOf"
	if _, err := NewProvider(cfg); err != nil {
		t.Fatalf("NewProvider(UseTLS, custom attrs): %v", err)
	}

	// An explicit port is preserved (skips the port-defaulting branch).
	cfg2 := baseCfg()
	cfg2.Port = 3389
	if _, err := NewProvider(cfg2); err != nil {
		t.Fatalf("NewProvider(explicit port): %v", err)
	}
}
