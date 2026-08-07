package auth

import (
	"context"
	"fmt"

	idoidc "github.com/sethbacon/terraform-suite-identity/identity/auth/oidc"

	"github.com/terraform-state-manager/terraform-state-manager/internal/config"
	"github.com/terraform-state-manager/terraform-state-manager/internal/egress"
)

// OIDCProvider is the shared identity OIDC provider, aliased so call sites refer
// to auth.OIDCProvider.
type OIDCProvider = idoidc.Provider

// NewOIDCProvider initialises an OIDC provider from config using a background
// context for discovery.
func NewOIDCProvider(cfg *config.OIDCConfig) (*OIDCProvider, error) {
	return NewOIDCProviderWithContext(context.Background(), cfg)
}

// NewOIDCProviderWithContext initialises an OIDC provider with the given context.
// AllowInsecureIssuer is set only in DEV_MODE, so local dev can use an http
// Keycloak issuer; HTTPS is required everywhere else, including production.
//
// EgressGuard carries the deployment's security.egress.allowlist. Since identity
// v0.25.0 every outbound request the provider makes — discovery, JWKS, and the
// credential-bearing token exchange — goes through it, and the token_endpoint
// and jwks_uri read OUT of the discovery document are validated against it
// before any request is built. It is a SEPARATE control from
// AllowInsecureIssuer: opting out of HTTPS for a dev issuer does not also opt
// out of knowing where the traffic goes, so a self-hosted IdP on an internal
// address must be allow-listed even in DEV_MODE.
func NewOIDCProviderWithContext(ctx context.Context, cfg *config.OIDCConfig) (*OIDCProvider, error) {
	if !cfg.Enabled {
		return nil, fmt.Errorf("OIDC is not enabled")
	}
	return idoidc.NewProviderWithContext(ctx, idoidc.Config{
		IssuerURL:           cfg.IssuerURL,
		ClientID:            cfg.ClientID,
		ClientSecret:        cfg.ClientSecret,
		RedirectURL:         cfg.RedirectURL,
		Scopes:              cfg.Scopes,
		AllowInsecureIssuer: IsDevMode(),
		EgressGuard:         egress.Guard(),
	})
}
