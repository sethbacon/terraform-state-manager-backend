package mtls

import (
	"crypto/x509"
	"crypto/x509/pkix"
	"testing"

	"github.com/terraform-state-manager/terraform-state-manager/internal/config"
)

func newProvider(t *testing.T, mappings []config.MTLSSubjectMapping) *Provider {
	t.Helper()
	p, err := NewProvider(config.MTLSConfig{Enabled: true, ClientCAFile: "/tmp/ca.pem", Mappings: mappings})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	return p
}

func TestAuthenticate_CN(t *testing.T) {
	p := newProvider(t, []config.MTLSSubjectMapping{{Subject: "CN=svc-drift", Scopes: []string{"state:drift"}}})
	// Case-insensitive: cert CN differs in case from the mapping.
	cert := &x509.Certificate{Subject: pkix.Name{CommonName: "SVC-Drift"}}
	subject, scopes, err := p.Authenticate(cert)
	if err != nil {
		t.Fatalf("expected match, got error: %v", err)
	}
	if subject != "CN=SVC-Drift" || len(scopes) != 1 || scopes[0] != "state:drift" {
		t.Fatalf("unexpected subject/scopes: %q %v", subject, scopes)
	}
}

func TestAuthenticate_SANDNS(t *testing.T) {
	p := newProvider(t, []config.MTLSSubjectMapping{{Subject: "dns:machine.example.com", Scopes: []string{"state:read"}}})
	cert := &x509.Certificate{
		Subject:  pkix.Name{CommonName: "irrelevant"},
		DNSNames: []string{"machine.example.com"},
	}
	_, scopes, err := p.Authenticate(cert)
	if err != nil || len(scopes) != 1 || scopes[0] != "state:read" {
		t.Fatalf("expected SAN-DNS match, got scopes=%v err=%v", scopes, err)
	}
}

func TestAuthenticate_FullDN(t *testing.T) {
	name := pkix.Name{CommonName: "svc-x", Organization: []string{"acme"}}
	dn := (&x509.Certificate{Subject: name}).Subject.String()
	p := newProvider(t, []config.MTLSSubjectMapping{{Subject: dn, Scopes: []string{"admin"}}})
	cert := &x509.Certificate{Subject: name}
	_, scopes, err := p.Authenticate(cert)
	if err != nil || len(scopes) != 1 || scopes[0] != "admin" {
		t.Fatalf("expected DN match, got scopes=%v err=%v", scopes, err)
	}
}

func TestAuthenticate_NoMatch(t *testing.T) {
	p := newProvider(t, []config.MTLSSubjectMapping{{Subject: "CN=known", Scopes: []string{"state:read"}}})
	cert := &x509.Certificate{Subject: pkix.Name{CommonName: "attacker"}}
	if _, _, err := p.Authenticate(cert); err == nil {
		t.Fatal("expected no-match error for unmapped subject")
	}
}

func TestAuthenticate_NilCert(t *testing.T) {
	p := newProvider(t, nil)
	if _, _, err := p.Authenticate(nil); err == nil {
		t.Fatal("expected error for nil certificate")
	}
}

func TestNewProvider_Disabled(t *testing.T) {
	if _, err := NewProvider(config.MTLSConfig{Enabled: false}); err == nil {
		t.Fatal("expected error when mTLS disabled")
	}
	if _, err := NewProvider(config.MTLSConfig{Enabled: true}); err == nil {
		t.Fatal("expected error when client_ca_file missing")
	}
}
