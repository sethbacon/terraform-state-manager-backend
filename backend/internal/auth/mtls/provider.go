// Package mtls provides mutual-TLS client-certificate authentication. When
// enabled, a client presenting a certificate that the TLS layer has VERIFIED
// against the configured client CA is authenticated and granted the scopes mapped
// to its subject. This is an additive, machine-to-machine auth method.
//
// SECURITY: this package never decides whether a certificate is trusted — that is
// the TLS handshake's job (ClientCAs + client-cert verification, see the server
// setup). The middleware only acts on certificates from a verified chain
// (tls.ConnectionState.VerifiedChains), so a merely-presented (unverified)
// certificate can never authenticate. Subject→scope mappings are admin-configured
// and never user-supplied.
package mtls

import (
	"crypto/x509"
	"fmt"
	"log/slog"
	"strings"

	"github.com/terraform-state-manager/terraform-state-manager/internal/config"
)

// Provider maps verified client-certificate subjects to scopes.
type Provider struct {
	mappings map[string][]string // normalized subject → scopes
}

// NewProvider builds an mTLS provider from configuration. The client CA pool is
// loaded by the TLS server (not here); this provider only maps subjects.
func NewProvider(cfg config.MTLSConfig) (*Provider, error) {
	if !cfg.Enabled {
		return nil, fmt.Errorf("mTLS is not enabled")
	}
	if cfg.ClientCAFile == "" {
		return nil, fmt.Errorf("auth.mtls.client_ca_file is required when mTLS is enabled")
	}
	if len(cfg.Mappings) == 0 {
		slog.Warn("mTLS enabled but no subject mappings configured — no certificate will be granted scopes")
	}
	m := make(map[string][]string, len(cfg.Mappings))
	for _, mapping := range cfg.Mappings {
		m[normalizeSubject(mapping.Subject)] = mapping.Scopes
		slog.Info("mTLS subject mapping registered", "subject", normalizeSubject(mapping.Subject), "scopes", mapping.Scopes)
	}
	return &Provider{mappings: m}, nil
}

// Authenticate resolves a VERIFIED client certificate to a subject + scopes. It
// tries, in order: the CN (CN=<cn>), each DNS SAN (a stronger identity than CN),
// then the full Distinguished Name. Returns an error when no mapping matches.
//
// The caller MUST pass a certificate from a verified chain; this method does not
// (and cannot) re-verify trust.
func (p *Provider) Authenticate(cert *x509.Certificate) (subject string, scopes []string, err error) {
	if cert == nil {
		return "", nil, fmt.Errorf("no client certificate provided")
	}

	candidates := make([]string, 0, 2+len(cert.DNSNames))
	candidates = append(candidates, "CN="+cert.Subject.CommonName)
	for _, dns := range cert.DNSNames {
		candidates = append(candidates, "dns:"+dns)
	}
	candidates = append(candidates, cert.Subject.String())

	for _, cand := range candidates {
		if scopes, ok := p.mappings[normalizeSubject(cand)]; ok {
			return cand, scopes, nil
		}
	}
	return "", nil, fmt.Errorf("no mTLS mapping for subject CN=%s (DN=%s)", cert.Subject.CommonName, cert.Subject.String())
}

// normalizeSubject lower-cases and trims a subject for case-insensitive matching.
func normalizeSubject(s string) string {
	return strings.TrimSpace(strings.ToLower(s))
}
