package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/terraform-state-manager/terraform-state-manager/internal/config"
	"github.com/terraform-state-manager/terraform-state-manager/internal/egress"
)

// discoveryServer spins up a minimal OIDC discovery document whose issuer
// matches its own URL (go-oidc requires the issuer in the document to match
// the configured issuer URL). Mirrors the identity module's own test helper.
func discoveryServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		doc := map[string]interface{}{
			"issuer":                 srv.URL,
			"authorization_endpoint": srv.URL + "/auth",
			"token_endpoint":         srv.URL + "/token",
			"jwks_uri":               srv.URL + "/keys",
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(doc)
	})
	return srv
}

// TestNewOIDCProviderWithContext_ProductionRejectsHTTPIssuer confirms
// RequireHTTPS is actually enforced outside DEV_MODE (issue #176): an http
// issuer must be rejected before any discovery attempt, so this needs no
// network access.
func TestNewOIDCProviderWithContext_ProductionRejectsHTTPIssuer(t *testing.T) {
	t.Setenv("DEV_MODE", "")

	_, err := NewOIDCProviderWithContext(context.Background(), &config.OIDCConfig{
		Enabled:      true,
		IssuerURL:    "http://issuer.example",
		ClientID:     "client",
		ClientSecret: "secret",
		RedirectURL:  "https://app.example/callback",
	})
	if err == nil {
		t.Fatal("expected an error for an http issuer outside DEV_MODE")
	}
	if !strings.Contains(err.Error(), "HTTPS") {
		t.Errorf("error = %q, want it to mention HTTPS", err.Error())
	}
}

// TestNewOIDCProviderWithContext_DevModeAllowsHTTPIssuer confirms local dev
// still works against a plaintext (e.g. Keycloak) discovery endpoint.
func TestNewOIDCProviderWithContext_DevModeAllowsHTTPIssuer(t *testing.T) {
	t.Setenv("DEV_MODE", "true")
	// DEV_MODE opts out of the HTTPS requirement only. Since identity v0.25.0
	// the DESTINATION rule is a separate control (the egress guard), and the
	// stub discovery endpoint is on loopback — so this must allow-list it
	// explicitly, exactly as a dev stack with a container-hosted IdP must.
	allowLoopbackEgress(t)

	srv := discoveryServer(t)
	defer srv.Close()

	_, err := NewOIDCProviderWithContext(context.Background(), &config.OIDCConfig{
		Enabled:      true,
		IssuerURL:    srv.URL,
		ClientID:     "client",
		ClientSecret: "secret",
		RedirectURL:  "https://app.example/callback",
	})
	if err != nil {
		t.Fatalf("NewOIDCProviderWithContext in DEV_MODE: %v", err)
	}
}

// allowLoopbackEgress points internal/egress at a guard that permits loopback
// for the duration of one test, restoring the strict default afterwards so a
// later test cannot inherit the widening.
func allowLoopbackEgress(t *testing.T) {
	t.Helper()
	if err := egress.Configure([]string{"127.0.0.1", "::1"}); err != nil {
		t.Fatalf("egress.Configure: %v", err)
	}
	t.Cleanup(func() { _ = egress.Configure(nil) })
}

// TestNewOIDCProviderWithContext_EgressGuardBlocksUnlistedInternalIssuer is the
// counterweight: without the allow-list entry the same DEV_MODE construction is
// REFUSED, naming the denied destination. This is the v0.25.0 behaviour a
// deployment feels first, and it is separate from AllowInsecureIssuer — opting
// out of HTTPS does not opt out of knowing where the traffic goes.
func TestNewOIDCProviderWithContext_EgressGuardBlocksUnlistedInternalIssuer(t *testing.T) {
	t.Setenv("DEV_MODE", "true")
	if err := egress.Configure(nil); err != nil {
		t.Fatalf("egress.Configure: %v", err)
	}

	srv := discoveryServer(t)
	defer srv.Close()

	_, err := NewOIDCProviderWithContext(context.Background(), &config.OIDCConfig{
		Enabled:      true,
		IssuerURL:    srv.URL,
		ClientID:     "client",
		ClientSecret: "secret",
		RedirectURL:  "https://app.example/callback",
	})
	if err == nil {
		t.Fatal("an IdP on an internal address must be refused when security.egress.allowlist does not name it")
	}
	if !strings.Contains(err.Error(), "egress") {
		t.Errorf("the refusal must name the egress policy so an operator knows what to configure; got %v", err)
	}
}

// TestNewOIDCProviderWithContext_ProductionAllowsHTTPSIssuer confirms the
// RequireHTTPS enforcement does not itself block a well-formed https issuer
// outside DEV_MODE (only the scheme is checked, not reachability).
func TestNewOIDCProviderWithContext_ProductionAllowsHTTPSIssuer(t *testing.T) {
	t.Setenv("DEV_MODE", "false")

	srv := discoveryServer(t)
	defer srv.Close()
	httpsIssuer := "https://" + strings.TrimPrefix(srv.URL, "http://")

	_, err := NewOIDCProviderWithContext(context.Background(), &config.OIDCConfig{
		Enabled:      true,
		IssuerURL:    httpsIssuer,
		ClientID:     "client",
		ClientSecret: "secret",
		RedirectURL:  "https://app.example/callback",
	})
	// Discovery itself will fail (the test server does not actually speak TLS
	// on that URL), but the error must come from discovery, not the
	// RequireHTTPS scheme check.
	if err == nil {
		t.Fatal("expected a discovery error (test server is not really TLS)")
	}
	if strings.Contains(err.Error(), "must use HTTPS") {
		t.Errorf("error = %q, RequireHTTPS should not reject an https:// issuer URL", err.Error())
	}
}

func TestNewOIDCProviderWithContext_Disabled(t *testing.T) {
	if _, err := NewOIDCProviderWithContext(context.Background(), &config.OIDCConfig{Enabled: false}); err == nil {
		t.Error("expected an error when OIDC is not enabled")
	}
}
