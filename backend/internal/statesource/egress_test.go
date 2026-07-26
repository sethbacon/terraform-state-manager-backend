package statesource

import (
	"os"
	"testing"

	identityhttpsafe "github.com/sethbacon/terraform-suite-identity/identity/httpsafe"
)

// TestMain relaxes the connector egress guard to also allow loopback so the
// connector tests (which dial httptest.NewServer on 127.0.0.1) can reach their
// stub servers. The production guard blocks loopback (see egress.go); that policy
// is asserted on a fresh guard in TestEgressGuard_Policy, independent of this
// relaxation.
func TestMain(m *testing.M) {
	_ = ConfigureEgress([]string{
		"127.0.0.1", "::1",
		"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16",
	})
	os.Exit(m.Run())
}

// TestEgressGuard_Policy verifies the real connector egress policy on a fresh
// guard: cloud metadata and loopback are blocked, RFC1918 private ranges are
// allowed (so internal state backends keep working) (#256).
func TestEgressGuard_Policy(t *testing.T) {
	g := identityhttpsafe.MustGuard("10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16")

	for _, u := range []string{
		"https://169.254.169.254/latest/meta-data/", // cloud metadata (link-local)
		"https://127.0.0.1/state",                    // loopback
	} {
		if err := g.ValidateURL(u); err == nil {
			t.Errorf("expected %q to be blocked by the egress policy", u)
		}
	}
	for _, u := range []string{
		"https://10.1.2.3/state",    // private (allow-listed)
		"https://192.168.1.5/state", // private (allow-listed)
	} {
		if err := g.ValidateURL(u); err != nil {
			t.Errorf("expected private %q to be allowed, got %v", u, err)
		}
	}
}

// TestValidateGitURL covers the git scheme restriction (#256): file://, http://
// and git:// are rejected; https and ssh are allowed. Uses literal IPs so no DNS
// lookup is needed.
func TestValidateGitURL(t *testing.T) {
	for _, u := range []string{
		"file:///etc/passwd",       // local file read
		"file://host/etc/passwd",   // local file read (with host)
		"http://internal/repo.git", // plaintext + internal
		"git://host/repo.git",      // plaintext
		"://nohost",                // unparseable / no host
	} {
		if err := validateGitURL(u); err == nil {
			t.Errorf("expected git repo_url %q to be rejected", u)
		}
	}
	for _, u := range []string{
		"https://10.1.2.3/org/repo.git", // https to a private range (allow-listed)
		"ssh://git@host/org/repo.git",   // ssh allowed
	} {
		if err := validateGitURL(u); err != nil {
			t.Errorf("expected git repo_url %q to be allowed, got %v", u, err)
		}
	}

	// https to the metadata endpoint is rejected via the guard's host check.
	if err := validateGitURL("https://169.254.169.254/repo.git"); err == nil {
		t.Error("expected https git repo_url to cloud metadata to be rejected")
	}
}

// TestNewAzure_AccountValidation covers the account charset restriction (#256):
// account is interpolated into the service-URL host, so anything outside the
// Azure storage-account charset (3-24 lowercase letters/digits) is rejected
// before the client is built.
func TestNewAzure_AccountValidation(t *testing.T) {
	key := "dGVzdGtleWZvcnVuaXR0ZXN0aW5nMTIz" // arbitrary valid base64
	for _, acct := range []string{
		"has-dash", "UPPERCASE", "ab", "a b", "acct/evil",
		"toolongaccountname123456789", "acct.evil",
	} {
		if _, err := newAzure(map[string]any{"account": acct, "container": "c"},
			map[string]any{"account_key": key}); err == nil {
			t.Errorf("azure account %q should be rejected", acct)
		}
	}
	if _, err := newAzure(map[string]any{"account": "validacct123", "container": "c"},
		map[string]any{"account_key": key}); err != nil {
		t.Errorf("valid azure account should be accepted, got %v", err)
	}
}
