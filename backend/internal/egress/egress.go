// Package egress owns the SSRF guard this application applies to the outbound
// requests made on its behalf by the shared terraform-suite-identity module:
// OIDC discovery, the JWKS key-set fetches and the authorization-code token
// exchange, the sibling-manifest discovery poll, and the module-freshness join
// that follows a sibling-asserted publicUrl.
//
// It exists because identity v0.25.0 routes every one of those through
// httpsafe.Guard and requires the deployment to supply one. Before that release
// the OIDC relying party built a bare &http.Client{} with Go's default
// cross-host redirect policy and used it to fetch the signing keys that decide
// which ID tokens are valid — and the token_endpoint and jwks_uri it used came
// out of the discovery document, so the issuer chose them.
//
// # Why this is not statesource's guard
//
// Both are built from the same operator setting (security.egress.allowlist /
// TSM_SECURITY_EGRESS_ALLOWLIST), and that is deliberate: one deployment, one
// statement of which internal destinations are legitimate. What differs is what
// an EMPTY setting means, because the two guards protect different things:
//
//   - statesource's connectors dial state BACKENDS, which are routinely
//     internal (an in-cluster Kubernetes API, an internal Consul, a private
//     HTTP backend), so an empty setting keeps the RFC1918/ULA private ranges
//     permitted and blocks only metadata/loopback/link-local.
//   - The requests here reach an IDENTITY PROVIDER and a SIBLING APP, both
//     operator-pinned by URL. An empty setting is therefore strict deny: a
//     deployment whose IdP or sibling lives on an internal address says so.
//
// A deployment that sets the allow-list REPLACES the connector default, so a
// value intended to admit an internal IdP must re-state the private ranges the
// connectors still need. See EgressConfig in internal/config.
package egress

import (
	"sync/atomic"

	identityhttpsafe "github.com/sethbacon/terraform-suite-identity/identity/httpsafe"
)

// guard holds the configured policy. A nil *Guard is a valid strict guard, so
// the zero value here already denies every internal destination — a process
// that never called Configure fails closed rather than open.
var guard atomic.Pointer[identityhttpsafe.Guard]

// Configure builds the guard from the operator's allow-list entries (hostnames,
// IPs or CIDR ranges). Call once at startup, before anything constructs an OIDC
// provider or a discovery client. An empty list yields the strict default.
func Configure(allowlist []string) error {
	g, err := identityhttpsafe.NewGuard(allowlist)
	if err != nil {
		return err
	}
	guard.Store(g)
	return nil
}

// Guard returns the configured guard, or the strict default (nil) when
// Configure has not run. It is safe to call from request goroutines: the setup
// wizard constructs OIDC providers at runtime.
func Guard() *identityhttpsafe.Guard { return guard.Load() }
