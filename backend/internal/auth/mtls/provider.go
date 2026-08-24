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

	"github.com/google/uuid"
	idauth "github.com/sethbacon/terraform-suite-identity/identity/auth"

	"github.com/terraform-state-manager/terraform-state-manager/internal/config"
)

// Provider maps verified client-certificate subjects to scopes.
type Provider struct {
	mappings map[string]subjectMapping // normalized subject → mapping
}

// subjectMapping is one configured certificate subject: the scopes it presents
// and, when it carries `admin`, the user whose carrier row decides whether that
// `admin` means anything on this request (issue #476).
type subjectMapping struct {
	scopes []string
	userID string
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
	m := make(map[string]subjectMapping, len(cfg.Mappings))
	for _, mapping := range cfg.Mappings {
		subject := normalizeSubject(mapping.Subject)

		// A repeated subject used to overwrite silently. That was untidy when a
		// mapping was only a scope list; now that one can name the user a
		// certificate acts as, a silent overwrite would bind a certificate to a
		// DIFFERENT PRINCIPAL than the line the operator is reading. Refuse.
		if _, dup := m[subject]; dup {
			return nil, fmt.Errorf("auth.mtls.mappings: subject %q is configured more than once; "+
				"remove the duplicate — a repeated subject would silently take the last scopes "+
				"and user_id in the file", mapping.Subject)
		}

		// REFUSE `admin` WITHOUT A USER (issue #476).
		//
		// Platform-admin authority lives in the platform_admins carrier and the
		// carrier is keyed on user_id, so an `admin` mapping with no user names
		// no grant that can be audited, and none that revoking a carrier row can
		// take away. Refusing at construction makes that unrepresentable rather
		// than merely discouraged: router.go turns this error into a startup
		// failure, so a deployment configured this way does not start.
		if err := idauth.ValidateProvisionableScopes(mapping.Scopes); err != nil && mapping.UserID == "" {
			return nil, fmt.Errorf("auth.mtls.mappings: subject %q carries the `admin` scope with no "+
				"user_id. Platform administration is held in the platform_admins carrier, which is "+
				"keyed on a user; set user_id to the UUID of the user this certificate acts as (and "+
				"grant them platform administration), or remove `admin` from the mapping",
				mapping.Subject)
		}

		if mapping.UserID != "" {
			if _, err := uuid.Parse(mapping.UserID); err != nil {
				return nil, fmt.Errorf("auth.mtls.mappings: subject %q has user_id %q, which is not a UUID: %w",
					mapping.Subject, mapping.UserID, err)
			}
		}

		m[subject] = subjectMapping{scopes: mapping.Scopes, userID: mapping.UserID}
		slog.Info("mTLS subject mapping registered",
			"subject", subject, "scopes", mapping.Scopes, "user_id", mapping.UserID)
	}
	return &Provider{mappings: m}, nil
}

// Authenticate resolves a VERIFIED client certificate to a subject + scopes. It
// tries, in order: the CN (CN=<cn>), each DNS SAN (a stronger identity than CN),
// then the full Distinguished Name. Returns an error when no mapping matches.
//
// The caller MUST pass a certificate from a verified chain; this method does not
// (and cannot) re-verify trust.
// The returned scopes are the CONFIGURED ones, not the effective ones. Anything
// reaching for `admin` here is reading a claim from a config file; the carrier
// decides whether it holds, and AuthMiddleware performs that resolution — so
// there is one place where an mTLS request's authority is settled, not two.
func (p *Provider) Authenticate(cert *x509.Certificate) (subject string, scopes []string, userID string, err error) {
	if cert == nil {
		return "", nil, "", fmt.Errorf("no client certificate provided")
	}

	candidates := make([]string, 0, 2+len(cert.DNSNames))
	candidates = append(candidates, "CN="+cert.Subject.CommonName)
	for _, dns := range cert.DNSNames {
		candidates = append(candidates, "dns:"+dns)
	}
	candidates = append(candidates, cert.Subject.String())

	for _, cand := range candidates {
		if mapping, ok := p.mappings[normalizeSubject(cand)]; ok {
			return cand, mapping.scopes, mapping.userID, nil
		}
	}
	return "", nil, "", fmt.Errorf("no mTLS mapping for subject CN=%s (DN=%s)", cert.Subject.CommonName, cert.Subject.String())
}

// normalizeSubject lower-cases and trims a subject for case-insensitive matching.
func normalizeSubject(s string) string {
	return strings.TrimSpace(strings.ToLower(s))
}
