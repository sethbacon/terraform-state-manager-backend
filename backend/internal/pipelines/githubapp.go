// githubapp.go mints GitHub installation access tokens for CI sources whose
// auth_method is "app": a GitHub App's RS256 JWT exchanged for an installation
// token.
//
// The exchange itself lives in terraform-suite-identity/identity/appcreds --
// see entra.go's header for why. What remains here is the cache policy and the
// explicit eviction the credential-rotation route depends on.
package pipelines

import (
	"context"

	sharedcreds "github.com/sethbacon/terraform-suite-identity/identity/appcreds"
)

// GitHubAppCreds is a GitHub App installation used to mint installation access
// tokens. An ALIAS, not a copy: internal/api builds these by name, and an alias
// guarantees the Fingerprint keying the cache is the shared package's own.
type GitHubAppCreds = sharedcreds.GitHubAppCreds

// ValidRSAPrivateKey reports whether pemStr parses as a supported RSA private
// key (PKCS#1 or PKCS#8) -- used to validate an uploaded App key before storing
// it. Re-exported rather than re-implemented so the check performed at upload is
// by construction the one performed at mint.
var ValidRSAPrivateKey = sharedcreds.ValidRSAPrivateKey

// MintGitHubInstallationToken returns a GitHub installation access token for the
// App installation in creds, minting and caching it until shortly before expiry.
// Concurrency-safe.
//
// Shares tokenCache with the Azure DevOps paths. That is safe because the
// fingerprints are namespaced per credential type, so a GitHub App entry can
// never be served for an Entra one -- see the shared package's
// TestFingerprint_NoCollisionAcrossCredentialTypes, which pins exactly that.
func MintGitHubInstallationToken(ctx context.Context, creds GitHubAppCreds) (string, error) {
	tok, err := sharedcreds.CachedMinter{Minter: sharedMinter(), Cache: tokenCache}.GitHubApp(ctx, creds)
	if err != nil {
		return "", err
	}
	return tok.AccessToken, nil
}

// ResetGitHubAppTokenCacheForTest clears the in-memory token cache between
// tests. The cache is shared with the Azure DevOps paths, so this and
// ResetEntraTokenCacheForTest do the same thing; both names are kept because
// each package's tests call the one that reads correctly there.
func ResetGitHubAppTokenCacheForTest() { tokenCache.Reset() }

// EvictGitHubAppTokenCacheKey removes a single cached token, keyed by
// GitHubAppCreds.Fingerprint. The GitHub-App counterpart of
// EvictADOTokenCacheKey, used by the same PUT /ci-sources/:id route for a
// GitHub App source's credential rotation. See EvictADOTokenCacheKey's comment
// for why the route evicts explicitly rather than relying on the fingerprint
// changing.
func EvictGitHubAppTokenCacheKey(key string) { tokenCache.EvictKey(key) }
