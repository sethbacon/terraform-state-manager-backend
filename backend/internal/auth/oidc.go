package auth

import (
	"context"
	"fmt"

	idoidc "github.com/sethbacon/terraform-suite-identity/identity/auth/oidc"

	"github.com/terraform-state-manager/terraform-state-manager/internal/config"
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
// RequireHTTPS is disabled so local dev can use an http Keycloak issuer; enable
// it (and an https issuer) in production.
func NewOIDCProviderWithContext(ctx context.Context, cfg *config.OIDCConfig) (*OIDCProvider, error) {
	if !cfg.Enabled {
		return nil, fmt.Errorf("OIDC is not enabled")
	}
	return idoidc.NewProviderWithContext(ctx, idoidc.Config{
		IssuerURL:    cfg.IssuerURL,
		ClientID:     cfg.ClientID,
		ClientSecret: cfg.ClientSecret,
		RedirectURL:  cfg.RedirectURL,
		Scopes:       cfg.Scopes,
		RequireHTTPS: false,
	})
}
