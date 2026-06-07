// Package driftingest provides a configurable OIDC bearer-token validator for
// the inbound code-drift ingest endpoint. The CI pipeline (e.g. Azure DevOps
// backed by Entra workload identity) presents an OIDC/ID token; this validator
// verifies the token's signature against the issuer's JWKS and checks the
// audience and expiry.
//
// This is intentionally separate from the interactive login OIDC provider: the
// ingest token comes from a different issuer (the workload-identity issuer), so
// it gets its own discovery + verifier instance and never shares the login
// provider. The validator performs no token exchange — it only verifies tokens.
package driftingest

import (
	"context"
	"fmt"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"
)

// Validator verifies inbound OIDC bearer tokens for the drift-ingest endpoint.
type Validator struct {
	verifier *oidc.IDTokenVerifier
	issuer   string
	audience string
}

// VerifiedToken carries the subject and resolved claims of a successfully
// verified token.
type VerifiedToken struct {
	Subject string
	// Scopes holds the values of the token's "scope" (space-delimited) or "scp"
	// (Entra, space-delimited) claim, if present. Empty when the token carries
	// no scope claim.
	Scopes []string
}

// NewValidator performs OIDC discovery against the issuer and builds a verifier
// that checks the token signature (via the issuer JWKS), issuer, audience, and
// expiry. The provided context governs the discovery request.
//
// issuer and audience are required; an empty issuer means the validator is not
// configured and the caller should reject ingest requests.
func NewValidator(ctx context.Context, issuer, audience string) (*Validator, error) {
	if issuer == "" {
		return nil, fmt.Errorf("drift-ingest OIDC issuer is required")
	}
	if audience == "" {
		return nil, fmt.Errorf("drift-ingest OIDC audience is required")
	}

	provider, err := oidc.NewProvider(ctx, issuer)
	if err != nil {
		return nil, fmt.Errorf("failed to discover drift-ingest OIDC issuer: %w", err)
	}

	verifier := provider.Verifier(&oidc.Config{
		ClientID: audience,
	})

	return &Validator{
		verifier: verifier,
		issuer:   issuer,
		audience: audience,
	}, nil
}

// NewValidatorWithVerifierForTest builds a Validator around a pre-constructed
// verifier, bypassing live OIDC discovery. It exists so tests (in this and other
// packages) can inject a verifier backed by a mock JWKS keyset.
func NewValidatorWithVerifierForTest(verifier *oidc.IDTokenVerifier, issuer, audience string) *Validator {
	return &Validator{verifier: verifier, issuer: issuer, audience: audience}
}

// Verify validates the raw bearer token and returns its verified subject and
// scopes. It fails on a bad signature, wrong issuer/audience, or expiry.
func (v *Validator) Verify(ctx context.Context, rawToken string) (*VerifiedToken, error) {
	if rawToken == "" {
		return nil, fmt.Errorf("empty token")
	}

	idToken, err := v.verifier.Verify(ctx, rawToken)
	if err != nil {
		return nil, fmt.Errorf("token verification failed: %w", err)
	}

	var claims struct {
		Subject string `json:"sub"`
		Scope   string `json:"scope"`
		Scp     string `json:"scp"`
	}
	if err := idToken.Claims(&claims); err != nil {
		return nil, fmt.Errorf("failed to parse token claims: %w", err)
	}

	return &VerifiedToken{
		Subject: idToken.Subject,
		Scopes:  parseScopes(claims.Scope, claims.Scp),
	}, nil
}

// parseScopes extracts space-delimited scope values from the standard "scope"
// claim, falling back to the Entra "scp" claim.
func parseScopes(scope, scp string) []string {
	raw := scope
	if raw == "" {
		raw = scp
	}
	if raw == "" {
		return nil
	}
	return strings.Fields(raw)
}
