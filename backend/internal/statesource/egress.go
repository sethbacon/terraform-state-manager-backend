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

// defaultEgressAllowlist exempts the RFC1918 private ranges from the deny-list so
// internal state backends (an in-cluster Kubernetes API, an internal Consul, a
// private HTTP backend) keep working, while cloud metadata, loopback, and
// link-local addresses stay blocked.
var defaultEgressAllowlist = []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"}

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
