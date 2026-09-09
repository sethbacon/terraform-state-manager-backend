// entra.go mints Azure DevOps access tokens for CI sources whose auth_method is
// "app": a Microsoft Entra app registration proved either by a client secret
// (OAuth 2.0 client-credentials) or by workload identity federation, where the
// platform projects a token and no secret is stored at all.
//
// # The exchanges are not here
//
// They live in terraform-suite-identity/identity/appcreds. terraform-registry
// had implemented the same mechanisms independently and the two copies drifted:
// this repository had workload identity federation first, registry had an SSRF
// egress guard this one lacked, and neither gained the other's until someone
// went looking (suite-identity#301).
//
// What remains here is this application's policy: the CISource row shape, the
// process-local token cache with its refresh margin, and the explicit eviction
// the credential-rotation route depends on.
package pipelines

import (
	"context"
	"strings"
	"sync"

	identityhttpsafe "github.com/sethbacon/terraform-suite-identity/identity/httpsafe"

	sharedcreds "github.com/sethbacon/terraform-suite-identity/identity/appcreds"
	"github.com/terraform-state-manager/terraform-state-manager/internal/egress"
)

// EntraCreds is a Microsoft Entra app registration used to mint Azure DevOps
// access tokens via client-credentials.
//
// An ALIAS, not a copy: internal/api builds these by name, and an alias keeps
// those call sites compiling while guaranteeing the Fingerprint that keys the
// cache is the same function the shared package uses.
type EntraCreds = sharedcreds.EntraCreds

// WorkloadIdentityCreds identifies a federated identity used to mint Azure
// DevOps tokens. There is no secret material at all -- just the client id; the
// tenant and the projected token come from the pod's own environment.
//
// The shared package calls this FederatedCreds. Aliased under the old name so
// the credential-rotation route keeps compiling.
type WorkloadIdentityCreds = sharedcreds.FederatedCreds

// ADOTokenCredential is the subset of azcore.TokenCredential a federated mint
// needs. Aliased so existing tests that build a fake keep working.
type ADOTokenCredential = sharedcreds.FederatedCredential

// entraTokenRefreshMargin re-mints this long before a cached token's expiry so
// an in-flight dispatch never races a hard expiry.
const entraTokenRefreshMargin = sharedcreds.DefaultRefreshMargin

// tokenCache is the process-local token cache, keyed by credential fingerprint
// so that rotating any field is self-invalidating.
//
// This stays here rather than moving to the shared package because caching is
// policy: terraform-registry persists its tokens sealed and bound to a database
// row, and a shared minter that cached silently would serve it a token its own
// store had already replaced.
var tokenCache = sharedcreds.NewCache(entraTokenRefreshMargin)

// entraLoginBaseURL is the Microsoft Entra login host. Still a package var, and
// still the test seam it always was: OverrideEntraLoginURLForTest sets it, and
// sharedMinter folds it into the minter it builds. githubAPIBaseURL is the same
// arrangement, declared in discovery.go because the repository and branch calls
// use it too.
var entraLoginBaseURL = sharedcreds.DefaultEntraLoginBaseURL

// testFederatedCredentialFactory substitutes the workload-identity credential.
// Nil in production, where the shared package's real azidentity factory is used.
var testFederatedCredentialFactory sharedcreds.FederatedCredentialFactory

// minter holds the built minter and the inputs it was built from.
//
// Rebuilt only when one of those inputs changes. Building one per mint would be
// correct but would discard the connection pool with it, opening a fresh TLS
// connection to the token endpoint on every dispatch; capturing one at init
// would pin whatever egress policy existed before egress.Configure ran, which
// for a process that configures an allow-list at startup is the strict default.
var (
	minterMu     sync.Mutex
	minterInst   *sharedcreds.Minter
	minterGuard  *identityhttpsafe.Guard
	minterLogin  string
	minterGitHub string
	minterFedSet bool
)

// sharedMinter returns the minter for the current egress policy and endpoints.
//
// The endpoint vars are read without a lock, exactly as the token exchanges read
// them before this package delegated: the override helpers are documented as
// unsafe for concurrent use with real traffic, and making them safe is a
// separate change from moving the exchanges.
func sharedMinter() *sharedcreds.Minter {
	g := egress.Guard()
	login, gh, fed := entraLoginBaseURL, githubAPIBaseURL, testFederatedCredentialFactory

	minterMu.Lock()
	defer minterMu.Unlock()
	// The GUARD comparison is load-bearing and tested: nothing invalidates this
	// cache when egress.Configure runs, so without it an operator's allow-list
	// would never take (TestSharedMinter_FollowsAnEgressReconfigure).
	//
	// The endpoint and factory comparisons are belt-and-braces: today the only
	// things that change them are the Override*ForTest helpers, which already
	// call invalidateMinter, so no test can distinguish them from their absence
	// and mutating them away survives. They are kept so that a future setter
	// that forgets to invalidate is merely redundant rather than broken.
	if minterInst != nil && minterGuard == g && minterLogin == login &&
		minterGitHub == gh && minterFedSet == (fed != nil) {
		return minterInst
	}

	// The guard is what this repository gains by delegating: these exchanges
	// carry a credential -- the client secret itself, or the RS256 app JWT --
	// and until now they went out on a bare http.Client with no egress policy at
	// all, while the rest of the process had one (suite-identity#301).
	opts := []sharedcreds.Option{
		sharedcreds.WithEgressGuard(g),
		sharedcreds.WithEntraLoginBaseURL(login),
		sharedcreds.WithGitHubAPIBaseURL(gh),
	}
	if fed != nil {
		opts = append(opts, sharedcreds.WithFederatedCredentialFactory(fed))
	}
	minterInst = sharedcreds.New(opts...)
	minterGuard, minterLogin, minterGitHub, minterFedSet = g, login, gh, fed != nil
	return minterInst
}

// invalidateMinter drops the cached minter after a seam changes.
func invalidateMinter() {
	minterMu.Lock()
	minterInst = nil
	minterMu.Unlock()
}

// OverrideEntraLoginURLForTest points the Entra token host at a test server and
// returns a restore func. Not safe for concurrent use with real traffic.
func OverrideEntraLoginURLForTest(u string) (restore func()) {
	old := entraLoginBaseURL
	if u != "" {
		entraLoginBaseURL = u
	}
	invalidateMinter()
	return func() {
		entraLoginBaseURL = old
		invalidateMinter()
	}
}

// OverrideWorkloadIdentityCredentialFactoryForTest points the federated mint at
// a fake credential and returns a restore func. Not safe for concurrent use
// with real traffic.
func OverrideWorkloadIdentityCredentialFactoryForTest(f func(clientID string) (ADOTokenCredential, error)) (restore func()) {
	old := testFederatedCredentialFactory
	testFederatedCredentialFactory = sharedcreds.FederatedCredentialFactory(f)
	invalidateMinter()
	return func() {
		testFederatedCredentialFactory = old
		invalidateMinter()
	}
}

// ResetEntraTokenCacheForTest clears the in-memory token cache between tests.
func ResetEntraTokenCacheForTest() { tokenCache.Reset() }

// MintEntraADOToken returns a bearer access token for Azure DevOps, minting via
// the Entra client-credentials grant and caching it until shortly before expiry.
// Concurrency-safe.
func MintEntraADOToken(ctx context.Context, creds EntraCreds) (string, error) {
	tok, err := sharedcreds.CachedMinter{Minter: sharedMinter(), Cache: tokenCache}.Entra(ctx, creds)
	if err != nil {
		return "", err
	}
	return tok.AccessToken, nil
}

// MintWorkloadIdentityADOToken returns a bearer access token for Azure DevOps,
// minting via workload identity's federated-token exchange -- same cache, same
// refresh margin, same concurrency guarantee as MintEntraADOToken, just a
// different credential source (no client secret ever exists for this method).
func MintWorkloadIdentityADOToken(ctx context.Context, clientID string) (string, error) {
	creds := WorkloadIdentityCreds{ClientID: strings.TrimSpace(clientID)}
	tok, err := sharedcreds.CachedMinter{Minter: sharedMinter(), Cache: tokenCache}.Federated(ctx, creds)
	if err != nil {
		return "", err
	}
	return tok.AccessToken, nil
}

// EvictADOTokenCacheKey removes a single cached token, keyed by the same
// Fingerprint EntraCreds and WorkloadIdentityCreds compute.
//
// ResetEntraTokenCacheForTest clears everything, for test isolation between
// unrelated cases. This clears exactly one row, for a real credential rotation:
// PUT /ci-sources/:id calls it with the fingerprint of the credential a source
// USED TO carry, so a token minted under that credential can never be served
// again once the row no longer has it. Rotating to a genuinely different value
// already gets this for free -- Fingerprint changes with it, so the old entry is
// simply never looked up again -- but a route whose whole job is "replace this
// row's credential" should not depend on that being true forever.
func EvictADOTokenCacheKey(key string) { tokenCache.EvictKey(key) }
