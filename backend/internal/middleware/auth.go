package middleware

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	idauth "github.com/sethbacon/terraform-suite-identity/identity/auth"
	idplatformadmin "github.com/sethbacon/terraform-suite-identity/identity/platformadmin"
	idstore "github.com/sethbacon/terraform-suite-identity/identity/store"

	"github.com/terraform-state-manager/terraform-state-manager/internal/approles"
	"github.com/terraform-state-manager/terraform-state-manager/internal/auth"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
	"github.com/terraform-state-manager/terraform-state-manager/internal/platformadmin"
)

// AuthCookieName is the HttpOnly cookie carrying the session JWT.
const AuthCookieName = "tsm_auth_token"

// APIKeyPrefix is the fixed prefix of TSM API keys (registry pattern: the
// first 10 characters of the full key are stored as the indexed lookup prefix).
const APIKeyPrefix = "tsm"

// AuthMiddleware validates the session JWT (from the Authorization header or the
// auth cookie), checks revocation, loads the user, and populates the request
// context with user_id, scopes (from claims, elevated by the platform-admin
// carrier), and jwt_claims. Header tokens that are not JWTs fall through to
// API-key authentication (registry order: JWT is stateless and tried first; keys
// cost a prefix lookup + bcrypt compare). orgRepo is used only on the API-key
// path, to cap a key's stored scopes by the owner's current combined scopes (see
// authenticateAPIKey). userRevocations enforces the per-user revoke-all watermark
// an authority reduction writes (#330); nil skips that check.
//
// platformAdmins is the per-request platform-admin elevation and may be nil (a
// deployment without a database connection, or a unit-test rig), in which case no
// elevation happens at all — the carrier can only ADD authority, so an unwired
// one withholds it rather than granting anything.
//
// THE ELEVATION IS ON THIS PATH AND NOT ON THE API-KEY PATH BELOW. That
// asymmetry is the design, not an omission: see authenticateAPIKey.
func AuthMiddleware(userRepo *idstore.UserRepository, tokenRepo *idstore.TokenRepository, apiKeyRepo *idstore.APIKeyRepository, orgRepo *approles.Members, userRevocations *repositories.UserTokenRevocationRepository, platformAdmins *platformadmin.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		// A verified mTLS client certificate (set by mtls.AuthMiddleware earlier in
		// the chain) already authenticated this request and populated scopes.
		if m, ok := c.Get("auth_method"); ok && m == "mtls" {
			c.Next()
			return
		}

		token, fromCookie := extractToken(c)
		if token == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "Missing authorization credentials"})
			return
		}

		claims, err := auth.ValidateJWT(token)
		if err != nil {
			// API keys arrive only via the Authorization header (never cookies).
			if !fromCookie && apiKeyRepo != nil && strings.HasPrefix(token, APIKeyPrefix+"_") &&
				authenticateAPIKey(c, apiKeyRepo, userRepo, orgRepo, token) {
				c.Next()
				return
			}
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "Invalid credentials"})
			return
		}

		if claims.JTI != "" && tokenRepo != nil {
			revoked, rErr := tokenRepo.IsTokenRevoked(c.Request.Context(), claims.JTI)
			if rErr != nil {
				c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Auth check failed"})
				return
			}
			if revoked {
				c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "Token has been revoked"})
				return
			}
		}

		// Per-user revoke-all watermark (#330). A JWT freezes its scopes at
		// login, so an authority reduction that happened after this token was
		// minted is invisible to it; the JTI denylist above cannot help because
		// the reduction knows no JTIs. Fail closed on a lookup error: a token
		// whose revocation status cannot be established must not be honoured.
		if userRevocations != nil && claims.IssuedAt != nil {
			revoked, rErr := userRevocations.TokensRevokedSince(c.Request.Context(), claims.UserID, claims.IssuedAt.Time)
			if rErr != nil {
				c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Auth check failed"})
				return
			}
			if revoked {
				c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "Token has been revoked"})
				return
			}
		}

		// A token naming a user who no longer exists is an AUTHENTICATION
		// failure (401), not a server fault. The sentinel is matched before the
		// generic error arm: since identity v0.24.0 a missing user arrives as an
		// error rather than (nil, nil), so a plain `if err != nil` would answer
		// 500 to every deleted-user token and leave the 401 below unreachable.
		// Platform-wide scope, deliberately: this IS the tenant check's own
		// prerequisite. Resolving the token's subject is authority DERIVATION —
		// there is no caller tenancy yet to scope by, because the scopes that
		// would define one are what this lookup exists to establish. Scoping it
		// to the token's own claims would let a forged claim decide which rows
		// authenticate it. Recorded in admin_audit_scope_test.go's reviewed list.
		user, err := userRepo.GetUserByID(c.Request.Context(), claims.UserID, idstore.OrgScopeAllOrganizations())
		if errors.Is(err, idstore.ErrNotFound) || (err == nil && user == nil) {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "User not found"})
			return
		}
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Failed to load user"})
			return
		}

		scopes, err := elevate(c.Request.Context(), platformAdmins, user.ID, claims.Scopes)
		if err != nil {
			// An authority question that did not resolve is not a completed
			// "no". Answering 403 here would silently downgrade a platform
			// administrator to a permission denial during exactly the incident
			// in which they need the admin surface, so this is a server fault —
			// the same answer the revocation checks above give when they cannot
			// establish their own precondition.
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Auth check failed"})
			return
		}

		setAuthContext(c, user.ID, claims, fromCookie, scopes)
		c.Next()
	}
}

// elevate resolves the caller's effective scopes through the platform-admin
// carrier, per request.
//
// PER REQUEST, NOT CACHED. One indexed read on a table with a handful of rows is
// what buys immediate revocation: a cache with any TTL at all reintroduces the
// window a long-lived session would have had, which is the whole hazard the
// carrier exists to close.
//
// A nil service means no carrier is wired, so the caller's own scopes stand
// unchanged. That is fail-closed in the direction that matters — the carrier only
// ever ADDS `admin` in this phase, so an absent one grants nothing.
func elevate(ctx context.Context, platformAdmins *platformadmin.Service, userID string, scopes []string) ([]string, error) {
	if scopes == nil {
		scopes = []string{}
	}
	if platformAdmins == nil {
		return scopes, nil
	}
	return platformAdmins.SessionScopes(ctx, userID, scopes)
}

// OptionalAuthMiddleware populates the auth context when a valid session token is
// present but never aborts. Used for endpoints like logout that are idempotent
// and must work even without (or with an expired) session. It honours the same
// revoke-all watermark as AuthMiddleware (#330), so a revoked session is treated
// as no session rather than as an authenticated one.
//
// It elevates through the same carrier, so the scope set it publishes cannot
// disagree with AuthMiddleware's. A carrier lookup FAILURE here leaves the
// session unelevated instead of aborting: this middleware's contract is that it
// never fails a request, and every route behind it is either unauthenticated-safe
// or idempotent.
func OptionalAuthMiddleware(userRepo *idstore.UserRepository, tokenRepo *idstore.TokenRepository, userRevocations *repositories.UserTokenRevocationRepository, platformAdmins *platformadmin.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		token, fromCookie := extractToken(c)
		if token == "" {
			c.Next()
			return
		}
		claims, err := auth.ValidateJWT(token)
		if err != nil {
			c.Next()
			return
		}
		if claims.JTI != "" && tokenRepo != nil {
			if revoked, rErr := tokenRepo.IsTokenRevoked(c.Request.Context(), claims.JTI); rErr == nil && revoked {
				c.Next()
				return
			}
		}
		if userRevocations != nil && claims.IssuedAt != nil {
			if revoked, rErr := userRevocations.TokensRevokedSince(c.Request.Context(), claims.UserID, claims.IssuedAt.Time); rErr != nil || revoked {
				c.Next()
				return
			}
		}
		// Authority derivation, as in AuthMiddleware above.
		if user, err := userRepo.GetUserByID(c.Request.Context(), claims.UserID, idstore.OrgScopeAllOrganizations()); err == nil && user != nil {
			scopes, eErr := elevate(c.Request.Context(), platformAdmins, user.ID, claims.Scopes)
			if eErr != nil {
				// Unelevated, never the token's claim re-read: an unresolved
				// authority question must not become an elevation.
				scopes = claims.Scopes
			}
			setAuthContext(c, user.ID, claims, fromCookie, scopes)
		}
		c.Next()
	}
}

// authenticateAPIKey resolves a Bearer token as an API key: indexed prefix
// lookup → bcrypt compare → expiry check → owning user must still exist → the
// key's stored scopes are capped by the owner's current combined scopes →
// `admin` stripped unconditionally → context populated. Capping (grantedSubset)
// makes a key's effective privileges track its owner across role downgrades and
// de-provisioning; without it a key minted while the owner held admin would keep
// admin after they lost it (#223). Last-used is recorded async so the request
// never blocks on the bookkeeping write.
//
// IT TAKES NO CARRIER, AND CANNOT. This function has no platformadmin.Service
// parameter, so the API-key path is structurally incapable of consulting the
// carrier — "a key must not inherit its owner's platform-admin" is enforced by
// there being nothing here to call, rather than by remembering not to call it. A
// key is a long-lived, often unattended credential, frequently held by CI; an
// elevation that rode along would hand every pipeline token the highest privilege
// in the product, revocable only by deleting the key.
func authenticateAPIKey(c *gin.Context, keys *idstore.APIKeyRepository, users *idstore.UserRepository, orgs *approles.Members, token string) bool {
	if len(token) < idauth.DisplayPrefixLength {
		return false
	}
	ctx := c.Request.Context()
	candidates, err := keys.GetAPIKeysByPrefix(ctx, token[:idauth.DisplayPrefixLength])
	if err != nil {
		return false
	}
	for _, k := range candidates {
		if !idauth.ValidateAPIKey(token, k.KeyHash) {
			continue
		}
		// The bcrypt hash matched: this IS the presented key. Expired or
		// orphaned keys deny outright (no fall-through to other candidates).
		if k.ExpiresAt != nil && time.Now().After(*k.ExpiresAt) {
			return false
		}
		userID := ""
		if k.UserID != nil {
			userID = strings.TrimSpace(*k.UserID)
		}

		// AN API KEY MUST BE TIED TO A MEMBER OF AN ORGANIZATION.
		//
		// Both halves below were previously skipped rather than refused, because
		// every check was gated on `userID != ""` with no else. A key with no
		// owner therefore skipped the owner-exists lookup AND the live-scope cap
		// under it, and authenticated at its mint-time scopes forever — the one
		// key in the system whose privileges tracked nobody (#438).
		//
		// A NOTE ON WHAT A NULL OWNER MEANS ELSEWHERE. In the shared identity
		// schema a NULL user_id means "organization SERVICE credential", and
		// terraform-registry's namespace authorizer deliberately exempts such a
		// key from any membership check (suite-identity 000007). That reading is
		// NOT adopted here: TSM's mintKey has always written an owner, so a
		// TSM-issued key with no owner is a legacy artifact rather than a service
		// credential, and this app has no notion of one to honour. Refusing at
		// this boundary leaves registry's own credentials untouched — it decides
		// its own — and refuses only what is presented to TSM.
		if userID == "" {
			// Logged because AuthMiddleware renders every false as a flat
			// "Invalid credentials"; without this an operator holding a refused
			// key has no way to tell this apart from a wrong secret.
			slog.Warn("api key refused: no owning user",
				"api_key_id", k.ID, "key_prefix", k.KeyPrefix, "organization_id", k.OrganizationID)
			return false
		}
		if strings.TrimSpace(k.OrganizationID) == "" {
			slog.Warn("api key refused: no owning organization", "api_key_id", k.ID)
			return false
		}

		if users != nil {
			// Authority derivation, as in AuthMiddleware: the key has already been
			// proved genuine by the bcrypt compare, and this only establishes that
			// its owner still exists before the live-scope cap below.
			user, uErr := users.GetUserByID(ctx, userID, idstore.OrgScopeAllOrganizations())
			if uErr != nil || user == nil {
				return false
			}
		}
		scopes := k.Scopes
		if scopes == nil {
			scopes = []string{}
		}
		// Cap the key's stored scopes by the owner's CURRENT combined scopes. Keys
		// record the scopes granted at mint time, so without this a later role
		// downgrade, org-membership removal, or IdP/SCIM de-provisioning would leave
		// the key authenticating at its original (possibly admin) scope (#223).
		// Re-deriving live scopes here makes a key's privileges track its owner
		// across every de-provisioning path. Fail closed on lookup error.
		if orgs != nil {
			// ...and the owner must still be a MEMBER of the organization the key
			// is bound to. Losing the membership is exactly the de-provisioning
			// event a stored scope list cannot see, and the cap below would not
			// catch it on its own: GetUserCombinedScopes unions across every
			// organization the owner still belongs to, so a user removed from A
			// but still in B keeps a full scope set and A's key keeps working.
			//
			// Fail closed on lookup error. CheckMembership deliberately does not
			// absorb one into "not a member" — a lookup that FAILED is not an
			// answer — so the two are told apart here rather than conflated.
			member, _, mErr := orgs.CheckMembership(ctx, k.OrganizationID, userID, idstore.OrgScopeAllOrganizations())
			if mErr != nil {
				return false
			}
			if !member {
				slog.Warn("api key refused: owner is not a member of the key's organization",
					"api_key_id", k.ID, "organization_id", k.OrganizationID)
				return false
			}

			live, sErr := orgs.GetUserCombinedScopes(ctx, userID)
			if sErr != nil {
				return false
			}
			scopes = grantedSubset(scopes, live)
		}
		// STRIPPED, not merely never added. `admin` is excluded from
		// assignableKeyScopes (#252) so no key minted through this API carries
		// it, but a key's stored scope set is not a live authority statement
		// about anybody — an older role model, a hand-written INSERT, or a seed
		// can put it there — and grantedSubset above will happily keep it when
		// the owner is an admin. KeyScopes is a free function that takes no
		// context, no connection and no user, so this line cannot become an
		// elevation no matter what it is handed.
		scopes = idplatformadmin.KeyScopes(scopes)
		c.Set("user_id", userID)
		c.Set("scopes", scopes)
		c.Set("auth_method", "apikey")
		c.Set("api_key_id", k.ID)
		// The organization the key was minted for. tenantscope reads this to
		// scope the request to the key's own organization rather than to the
		// union of its owner's memberships (#459) — a key acts where it was
		// minted, not everywhere its owner can reach.
		//
		// From the key ROW, never from a header: an organization the caller
		// could supply would let a key choose its own scope.
		c.Set("api_key_organization_id", k.OrganizationID)
		// The organization the key was minted for. tenantscope reads this to
		// scope the request to the key's own organization rather than to the
		// union of its owner's memberships (#459) — a key acts where it was
		// minted, not everywhere its owner can reach.
		//
		// From the key ROW, never from a header: an organization the caller
		// could supply would let a key choose its own scope.

		go func(id string) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = keys.UpdateLastUsed(ctx, id)
		}(k.ID)
		return true
	}
	return false
}

// grantedSubset returns the key scopes still granted by the owner's live scope
// set, using the same hierarchical HasScope semantics as the rest of authz (so
// an admin owner still grants every finer scope, while a downgraded owner drops
// the ones they no longer hold). Order is preserved.
func grantedSubset(keyScopes, live []string) []string {
	out := make([]string, 0, len(keyScopes))
	for _, s := range keyScopes {
		if auth.HasScope(live, auth.Scope(s)) {
			out = append(out, s)
		}
	}
	return out
}

func extractToken(c *gin.Context) (token string, fromCookie bool) {
	if authHeader := c.GetHeader("Authorization"); strings.HasPrefix(authHeader, "Bearer ") {
		return strings.TrimSpace(strings.TrimPrefix(authHeader, "Bearer ")), false
	}
	if v, err := c.Cookie(AuthCookieName); err == nil && v != "" {
		return v, true
	}
	return "", false
}

// setAuthContext publishes the authenticated principal and its EFFECTIVE scopes.
//
// scopes is passed in rather than read from claims.Scopes here, because by this
// point they are no longer the same thing: the carrier has been consulted and
// may have added `admin` that the token never carried. Reading the claims again
// at this last step is precisely how an elevation gets computed and then
// discarded.
func setAuthContext(c *gin.Context, userID string, claims *auth.Claims, fromCookie bool, scopes []string) {
	c.Set("user_id", userID)
	c.Set("jwt_claims", claims)
	if fromCookie {
		c.Set("auth_method", "jwt_cookie")
	} else {
		c.Set("auth_method", "jwt")
	}
	if scopes == nil {
		scopes = []string{}
	}
	c.Set("scopes", scopes)
}
