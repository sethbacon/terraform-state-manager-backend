package driftingest

import (
	"net"
	"net/url"
	"strings"
)

// CanonicalHost normalizes a registry host so the suite "Consumed by" join
// compares like-for-like across apps. The host captured from a Terraform module
// source address, the registry's service-discovery host, and the registry's own
// public host can differ only in case, a default port, a trailing FQDN dot, or
// an accidental scheme prefix; folding those away makes the exact-match join
// robust to such variants. Exported so the read path (internal/api) shares this
// exact rule with the capture path here.
//
// It strips any scheme, lowercases the host, removes a trailing dot, and drops
// a default port (:80/:443) while preserving any non-default port. IDN/punycode
// folding is intentionally NOT done here — it is deferred to the shared helper.
//
// TODO(stage2): replace with the shared suite.CanonicalHost helper once the
// terraform-suite-identity module exposes it (a byte-identical copy lives in
// terraform-registry-backend internal/api).
func CanonicalHost(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	// If a scheme slipped in (e.g. the value came from a full URL), keep only
	// the authority component.
	if strings.Contains(raw, "://") {
		if u, err := url.Parse(raw); err == nil && u.Host != "" {
			raw = u.Host
		}
	}
	host, port := raw, ""
	if h, p, err := net.SplitHostPort(raw); err == nil {
		host, port = h, p
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if port == "" || port == "80" || port == "443" {
		return host
	}
	return net.JoinHostPort(host, port)
}
