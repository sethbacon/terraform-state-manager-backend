package statesource

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	identityhttpsafe "github.com/sethbacon/terraform-suite-identity/identity/httpsafe"
)

// TestMain relaxes the connector egress guard to also allow loopback so the
// connector tests (which dial httptest.NewServer on 127.0.0.1) can reach their
// stub servers, and permits the OS temp directory as a local state-source root
// so tests that build a local connector over t.TempDir() clear the containment
// check in roots.go. The production guard blocks loopback (see egress.go) and
// permits no roots at all; both policies are asserted independently — in
// TestEgressGuard_Policy on a fresh guard, and in TestLocalBasePathContainment
// on roots configured per case.
func TestMain(m *testing.M) {
	_ = ConfigureEgress([]string{
		"127.0.0.1", "::1",
		"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16",
	})
	_ = ConfigureLocalRoots([]string{os.TempDir()})
	os.Exit(m.Run())
}

// TestEgressGuard_Policy verifies the real connector egress policy on a fresh
// guard: cloud metadata and loopback are blocked, RFC1918 private ranges are
// allowed (so internal state backends keep working) (#256).
func TestEgressGuard_Policy(t *testing.T) {
	g := identityhttpsafe.MustGuard("10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16")

	for _, u := range []string{
		"https://169.254.169.254/latest/meta-data/", // cloud metadata (link-local)
		"https://127.0.0.1/state",                   // loopback
	} {
		if err := g.ValidateURL(context.Background(), u); err == nil {
			t.Errorf("expected %q to be blocked by the egress policy", u)
		}
	}
	for _, u := range []string{
		"https://10.1.2.3/state",    // private (allow-listed)
		"https://192.168.1.5/state", // private (allow-listed)
	} {
		if err := g.ValidateURL(context.Background(), u); err != nil {
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

// TestConsulSafeClient_StripsTokenCrossHost verifies the consul client removes the
// non-standard X-Consul-Token header when a redirect crosses to a different host
// (net/http forwards unknown headers across redirects, which would leak the ACL
// token) while preserving it on a same-host redirect (#302). Literal private IPs
// keep the guard's own re-validation passing (so the strip path is exercised) and
// avoid any DNS lookup.
func TestConsulSafeClient_StripsTokenCrossHost(t *testing.T) {
	check := consulSafeClient().CheckRedirect
	orig := httptest.NewRequest(http.MethodGet, "https://10.1.2.3/v1/kv/x", nil)

	cross := httptest.NewRequest(http.MethodGet, "https://10.9.9.9/v1/kv/x", nil)
	cross.Header.Set("X-Consul-Token", "secret")
	_ = check(cross, []*http.Request{orig})
	if cross.Header.Get("X-Consul-Token") != "" {
		t.Error("X-Consul-Token must be stripped on a cross-host redirect")
	}

	same := httptest.NewRequest(http.MethodGet, "https://10.1.2.3/v1/kv/y", nil)
	same.Header.Set("X-Consul-Token", "secret")
	_ = check(same, []*http.Request{orig})
	if same.Header.Get("X-Consul-Token") != "secret" {
		t.Error("X-Consul-Token must be preserved on a same-host redirect")
	}
}

// TestSafeGitHTTPClient_NoTimeout guards that the git-clone client carries no
// fixed request timeout — clone duration is bounded by the clone context, and a
// connector-length cap would truncate a large upload-pack (#302).
func TestSafeGitHTTPClient_NoTimeout(t *testing.T) {
	if got := safeGitHTTPClient().Timeout; got != 0 {
		t.Errorf("git clone client timeout = %v, want 0", got)
	}
}

// TestInstallGuardedGitTransport installs the process-global guarded git https
// transport; it must not panic (dial-time guard follow-up to #256, #302).
func TestInstallGuardedGitTransport(t *testing.T) {
	InstallGuardedGitTransport()
}

// TestEgressGuard_AllowsIPv6ULA verifies the default connector allow-list also
// exempts the IPv6 ULA range (fc00::/7) so IPv6-only internal backends work,
// mirroring the IPv4 RFC1918 exemption, while IPv6 link-local stays blocked (#302).
func TestEgressGuard_AllowsIPv6ULA(t *testing.T) {
	g := identityhttpsafe.MustGuard(defaultEgressAllowlist...)
	if err := g.ValidateURL(context.Background(), "https://[fd00::1]/state"); err != nil {
		t.Errorf("expected IPv6 ULA backend to be allowed, got %v", err)
	}
	if err := g.ValidateURL(context.Background(), "https://[fe80::1]/state"); err == nil {
		t.Error("expected IPv6 link-local to be blocked")
	}
}
