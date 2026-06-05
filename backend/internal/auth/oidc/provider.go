// Package oidc - provider.go delegates generic OpenID Connect handling to the
// shared identity/auth/oidc package, keeping TSM's config mapping and the
// "enabled" gate local to the app.
package oidc

import (
	"context"
	"fmt"

	identityoidc "github.com/sethbacon/terraform-suite-identity/identity/auth/oidc"

	"github.com/terraform-state-manager/terraform-state-manager/internal/config"
)

// OIDCProvider is the suite identity OIDC provider, aliased so existing call
// sites keep referring to oidc.OIDCProvider.
type OIDCProvider = identityoidc.Provider

// NewOIDCProvider initializes a new OIDC provider using a background context.
func NewOIDCProvider(cfg *config.OIDCConfig) (*OIDCProvider, error) {
	return NewOIDCProviderWithContext(context.Background(), cfg)
}

// NewOIDCProviderWithContext initializes a new OIDC provider with the given
// context. The "enabled" gate stays in the app; the shared package only needs
// the resolved connection settings.
func NewOIDCProviderWithContext(ctx context.Context, cfg *config.OIDCConfig) (*OIDCProvider, error) {
	if !cfg.Enabled {
		return nil, fmt.Errorf("OIDC is not enabled")
	}

	return identityoidc.NewProviderWithContext(ctx, identityoidc.Config{
		IssuerURL:    cfg.IssuerURL,
		ClientID:     cfg.ClientID,
		ClientSecret: cfg.ClientSecret,
		RedirectURL:  cfg.RedirectURL,
		Scopes:       cfg.Scopes,
	})
}
