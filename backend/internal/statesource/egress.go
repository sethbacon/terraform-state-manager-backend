// egress.go routes the state-source connectors that dial an operator-supplied
// host with an attached credential (http, consul, k8s, git) through an SSRF
// egress guard (#256). Without this a sources:manage holder who is not a network
// administrator could point a connector at the cloud metadata endpoint
// (169.254.169.254) or an internal service and use this server — which sits in a
// privileged network — as an SSRF proxy.
//
// Unlike outbound notifications (external webhooks, strict default that denies
// ALL private ranges), state backends are frequently INTERNAL — an in-cluster
// Kubernetes API, an internal Consul, a private HTTP backend — so the RFC1918
// private ranges are allow-listed to avoid breaking legitimate internal backends.
// What stays blocked is the highest-value target set: the cloud metadata endpoint
// and other link-local addresses, loopback, and non-private reserved ranges,
// enforced at dial time (resolve-and-pin, so a DNS answer cannot change between
// check and connect) with per-hop redirect re-validation and cross-origin
// credential stripping. Tightening this to an operator-configured allow-list
// (to also block internal-private SSRF without breaking internal backends) is
// tracked as a follow-up.
package statesource

import (
	"net/http"
	"time"

	identityhttpsafe "github.com/sethbacon/terraform-suite-identity/identity/httpsafe"
)

const connectorHTTPTimeout = 30 * time.Second

// defaultEgressAllowlist exempts the RFC1918 (IPv4) and ULA (IPv6, fc00::/7)
// private ranges from the deny-list so internal state backends (an in-cluster
// Kubernetes API, an internal Consul, a private HTTP backend) keep working —
// including IPv6-only internal deployments — while cloud metadata, loopback, and
// link-local addresses stay blocked.
var defaultEgressAllowlist = []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "fc00::/7"}

// egressGuard denies loopback and link-local (including the 169.254.169.254 cloud
// metadata endpoint) and other non-private reserved ranges, while allow-listing
// the RFC1918 private ranges so internal state backends keep working.
var egressGuard = identityhttpsafe.MustGuard(defaultEgressAllowlist...)

// ConfigureEgress rebuilds the connector egress guard from allowlist entries
// (hostnames, IPs, or CIDR ranges that are exempt from the deny-list). Call once
// at startup to widen the default RFC1918 allow-list to an operator's own
// internal ranges; also used by tests to allow loopback for httptest stub
// backends. Not safe to call concurrently with connector construction.
func ConfigureEgress(allowlist []string) error {
	g, err := identityhttpsafe.NewGuard(allowlist)
	if err != nil {
		return err
	}
	egressGuard = g
	return nil
}

// safeHTTPClient returns an SSRF-guarded HTTP client for connectors that need no
// custom transport of their own (http backend, consul).
func safeHTTPClient() *http.Client {
	return identityhttpsafe.NewClient(connectorHTTPTimeout, egressGuard)
}

// consulSafeClient is safeHTTPClient plus a redirect hook that strips consul's
// non-standard X-Consul-Token header on a cross-host hop. net/http auto-strips
// the credential headers it knows about (Authorization, Cookie, …) when a
// redirect crosses to a different host, but forwards unknown ones like
// X-Consul-Token — so without this the ACL token could ride a backend 302 to
// another host and leak (#302). The guard's own per-hop re-validation still runs.
func consulSafeClient() *http.Client {
	c := safeHTTPClient()
	guardCheck := c.CheckRedirect
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) > 0 && req.URL.Host != via[0].URL.Host {
			req.Header.Del("X-Consul-Token")
		}
		if guardCheck != nil {
			return guardCheck(req, via)
		}
		return nil
	}
	return c
}

// safeGitHTTPClient is the git-clone variant of safeHTTPClient: the same
// dial-time egress guard, but no fixed request timeout. A clone's duration is
// bounded by the caller's context (the /sources RequestTimeout middleware), and
// a fixed connector timeout would truncate a large upload-pack.
func safeGitHTTPClient() *http.Client {
	return identityhttpsafe.NewClient(0, egressGuard)
}
