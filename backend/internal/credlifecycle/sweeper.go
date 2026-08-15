// Package credlifecycle centralises the invalidation of every credential family
// that carries a *snapshot* of a principal's derived authority (issue #330).
//
// This app issues two credential families whose authority is frozen at issue
// time rather than re-derived from the database on every request:
//
//   - JWT sessions — the scope list is embedded in the claims at login
//     (auth.GenerateJWT) and is never re-read; only a JTI denylist hit or the
//     per-user revoke-all watermark
//     (repositories.UserTokenRevocationRepository) stops one.
//   - API keys — the scope list AND the owning organization_id are stored on the
//     api_keys row at creation. middleware.authenticateAPIKey caps the stored
//     scopes by the owner's live combined scopes, which bounds what a stale key
//     can DO, but the row itself survives every authority reduction until
//     something deletes it.
//
// It follows that any event which REDUCES a principal's derived authority —
// removal from an organization, reassignment of their role template, or
// IdP-driven deprovisioning via SCIM or a group-mapping sync — must invalidate
// BOTH families, or the reduction is cosmetic for whichever family it forgot.
// This package exists so the sweep is one call with one meaning rather than a
// fragment re-derived at each call site.
//
// HOW THIS DIFFERS FROM THE REGISTRY'S EQUIVALENT. terraform-registry-backend
// scopes its API-key sweep to one organization (ListByUserAndOrganization),
// because a registry key's organization_id is a real authority binding. Here it
// is not: apikeys.mintKey tags EVERY key with the global default organization
// (see APIKeysHandlers.keysVisibleToScope, #182), and scopes are checked
// against the owner's cross-organization union (GetUserCombinedScopes). So an
// org-scoped filter would silently skip the very keys a membership removal
// invalidates. The sweep is therefore keyed on the owner and filtered by the
// authority they RETAIN across all organizations after the change.
//
// THIS IS WHY THE SWEEP PASSES OrgScopeAllOrganizations(). identity v0.25.0
// pairs the membership strip with the credential sweep by feeding the strip's
// returned OrgScope — the organizations whose membership it actually removed —
// straight into RevokeAPIKeysForUser, so a key is revoked exactly where
// authority was just withdrawn (its #160 vs #732/#736). That pairing assumes
// api_keys.organization_id names the organization whose authority the key draws
// on. In TSM it names the default organization for every key ever minted, so
// binding the sweep to the strip's result would revoke NOTHING for a user whose
// stripped organizations do not happen to include the default one — reinstating
// the stranded-credential defect the sweep exists to prevent. The invariant the
// pairing encodes ("sweep exactly where authority was withdrawn") is satisfied
// here on the PRINCIPAL axis instead, which is the axis a TSM key is actually
// bound to: authority was withdrawn from this user, so this user's keys go.
package credlifecycle

import (
	"context"
	"errors"
	"log/slog"

	idstore "github.com/sethbacon/terraform-suite-identity/identity/store"

	"github.com/terraform-state-manager/terraform-state-manager/internal/approles"
	"github.com/terraform-state-manager/terraform-state-manager/internal/auth"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
)

// AuthorityRetained reports whether every scope in have is still granted by
// retained — i.e. whether a credential frozen with `have` asks for no more than
// the principal currently holds.
//
// This is the whole difference between "the authority changed" and "the
// authority was REDUCED", and the sweep must key off the latter. Revoking an
// API key is irreversible (the secret is displayed once at creation and cannot
// be recovered), so a sweep triggered by an authority INCREASE, or by a
// re-application of an unchanged role on an ordinary SSO login, would destroy
// working credentials fleet-wide for no security benefit.
//
// Comparison is by scope semantics, not slice identity: auth.HasScope resolves
// the "admin" wildcard and the read/write implications, so ["state:read"] is
// retained under ["state:write"], and everything is retained under ["admin"].
// An empty `retained` grants nothing, so any credential carrying at least one
// scope is not retained; a credential with no scopes grants nothing and is
// vacuously retained.
func AuthorityRetained(have, retained []string) bool {
	for _, s := range have {
		if !auth.HasScope(retained, auth.Scope(s)) {
			return false
		}
	}
	return true
}

// Sweeper invalidates the credential families derived from an authority that
// has just been reduced.
//
// A nil *Sweeper is a valid no-op receiver: handler sets constructed without the
// revocation subsystem wired (unit-test rigs, and any deployment whose schema
// predates the user_token_revocations migration) keep their previous behaviour
// instead of panicking or issuing unexpected queries.
type Sweeper struct {
	// userRevocations moves the per-user JWT revoke-all watermark. Lives on the
	// app's own connection.
	userRevocations *repositories.UserTokenRevocationRepository
	// apiKeys revokes API-key rows. Lives on the identity connection.
	apiKeys *idstore.APIKeyRepository
	// orgs re-derives the authority the principal RETAINS after the change, so
	// only keys that now over-ask are revoked.
	orgs *approles.Members
}

// NewSweeper builds a Sweeper. Any repository may be nil, in which case the
// corresponding half is not swept; if every one is nil the constructor returns
// nil so callers can store the result directly and rely on the no-op receiver.
func NewSweeper(userRevocations *repositories.UserTokenRevocationRepository, apiKeys *idstore.APIKeyRepository, orgs *approles.Members) *Sweeper {
	if userRevocations == nil && apiKeys == nil {
		return nil
	}
	return &Sweeper{userRevocations: userRevocations, apiKeys: apiKeys, orgs: orgs}
}

// Outcome reports what a sweep actually managed to invalidate. Every sweep is
// best-effort: by the time it runs the authority change has already committed,
// so a failure is reported and logged rather than rolled back — turning it into
// an error response would falsely tell the caller that the (already applied)
// change did not happen. Incomplete lets a handler surface "the reduction landed
// but the credential sweep did not" to its caller.
type Outcome struct {
	TokensRevoked bool
	KeysRevoked   int
	// KeysRetained counts keys the sweep deliberately left alone because every
	// scope they carry is still granted by the principal's remaining authority.
	KeysRetained int
	Incomplete   bool
}

// AuthorityReduced invalidates the credentials a user derives from a membership
// that has just been removed, or from a role template that has just been
// reassigned or narrowed: their JWT sessions (which carry the scope union that
// included the lost authority) and every API key whose frozen scopes the user no
// longer holds.
//
// Call it AFTER the authority change has committed — the retained set is
// re-derived from the database, so calling it before would compare against the
// authority being taken away.
func (s *Sweeper) AuthorityReduced(ctx context.Context, userID, reason string) Outcome {
	if s == nil {
		return Outcome{}
	}
	out := s.revokeTokens(ctx, userID, reason)
	keyOut := s.revokeOverAskingKeys(ctx, userID, reason)
	out.KeysRevoked = keyOut.KeysRevoked
	out.KeysRetained = keyOut.KeysRetained
	out.Incomplete = out.Incomplete || keyOut.Incomplete
	return out
}

// KeysOnly invalidates only the API keys a user can no longer justify,
// deliberately leaving the JWT watermark untouched.
//
// This exists for the IdP login paths (OIDC / SAML / LDAP group-mapping
// reconciliation). That reconciliation runs a few hundred microseconds BEFORE
// the same request mints the user's new session JWT, and RevokeAllUserTokens
// writes a full-precision NOW() watermark while a JWT's iat is floored to the
// second (RFC 7519). Moving the watermark there would make the freshly minted
// token compare as revoked — TokensRevokedSince deliberately resolves that
// same-second ambiguity toward "revoked" — and the user could never log in.
//
// EXACTLY WHAT THIS COVERS, AND WHAT IT DOES NOT.
//
// Covered: the token minted by THIS request. It is issued from
// GetUserCombinedScopes AFTER the membership change committed, so it already
// carries the reduced authority; moving the watermark would buy nothing and
// would break login.
//
// NOT covered — the residual: the user's OTHER live sessions, minted at EARLIER
// logins. Those carry the pre-reduction scope union and nothing here retires
// them; they stay valid until their own 24h TTL expires. The exposure is bounded
// by that TTL, whereas the API-key family — the one this call does sweep — would
// otherwise survive the deprovisioning indefinitely. An operator who needs an
// IdP-driven reduction to retire every existing session immediately must take
// the administrative path (the /admin/organizations/:id/members routes), which
// calls AuthorityReduced and does move the watermark.
func (s *Sweeper) KeysOnly(ctx context.Context, userID, reason string) Outcome {
	if s == nil {
		return Outcome{}
	}
	return s.revokeOverAskingKeys(ctx, userID, reason)
}

// UserDeprovisioned invalidates every credential the user holds: JWT sessions
// plus every API key, unconditionally.
//
// Use for whole-principal offboarding — SCIM DELETE /Users/{id}, SCIM
// active=false via PUT or PATCH, admin user deletion, GDPR erasure — where the
// user retains no authority at all and no scope comparison is needed.
//
// Since identity v0.25.0 this is ONE bulk statement rather than a list plus a
// RevokeAPIKey per row. That removes the window in which a key minted between
// the list and the last delete survived the sweep, and it removes the
// per-key already-gone race entirely: a DELETE that matches nothing is a
// count of zero, not an error to classify. See the package doc for why the
// scope is platform-wide here.
func (s *Sweeper) UserDeprovisioned(ctx context.Context, userID, reason string) Outcome {
	if s == nil {
		return Outcome{}
	}
	out := s.revokeTokens(ctx, userID, reason)
	if s.apiKeys == nil {
		return out
	}
	n, err := s.apiKeys.RevokeAPIKeysForUser(ctx, userID, idstore.OrgScopeAllOrganizations())
	if err != nil {
		slog.Error("credlifecycle: failed to revoke user API keys",
			"user_id", userID, "reason", reason, "error", err)
		out.Incomplete = true
		return out
	}
	out.KeysRevoked = int(n)
	if n > 0 {
		slog.Info("credlifecycle: API keys revoked", "count", n, "user_id", userID, "reason", reason)
	}
	return out
}

// alreadyGone reports whether a revocation failed because the key had already
// been deleted — by a concurrent rotation, a parallel sweep, or an admin acting
// between revokeOverAskingKeys' listing and its delete.
//
// It must NOT count as Incomplete. Outcome.Incomplete is a hard failure signal:
// AdminHandlers.DeleteUser and EraseUser turn it into a 500 and refuse to
// proceed, so treating a raced key as a failure would make offboarding a user
// fail precisely because one of their credentials was already destroyed — the
// desired end state. Since identity v0.24.0 RevokeAPIKey reports the zero-row
// delete as ErrNotFound instead of nil, so this arm is what keeps the sweep
// idempotent across the bump.
func alreadyGone(err error) bool { return errors.Is(err, idstore.ErrNotFound) }

func (s *Sweeper) revokeTokens(ctx context.Context, userID, reason string) Outcome {
	if s.userRevocations == nil {
		return Outcome{}
	}
	if err := s.userRevocations.RevokeAllUserTokens(ctx, userID); err != nil {
		slog.Error("credlifecycle: failed to revoke user tokens after authority reduction",
			"user_id", userID, "reason", reason, "error", err)
		return Outcome{Incomplete: true}
	}
	return Outcome{TokensRevoked: true}
}

// revokeOverAskingKeys revokes the API keys owned by userID whose frozen scopes
// are no longer covered by the authority the user retains after the change.
//
// Revocation (rather than scope narrowing) is the mechanism, matching the
// posture for the JWT family, which is likewise invalidated wholesale rather
// than re-scoped. But the mechanism is irreversible — a key's secret is
// displayed once at creation and cannot be recovered — so it is applied per key
// and only where the key actually over-asks. Without that filter, an authority
// INCREASE, or the ordinary re-application of an unchanged IdP group mapping on
// every login, would hard-delete working keys fleet-wide.
func (s *Sweeper) revokeOverAskingKeys(ctx context.Context, userID, reason string) Outcome {
	if s.apiKeys == nil {
		return Outcome{}
	}
	if s.orgs == nil {
		// The retained set is what makes revocation safe; without it the only
		// options are "revoke nothing" and "revoke everything", and destroying
		// unrecoverable credentials because a repository was not wired is the
		// worse failure. Reported as incomplete so it is not silent.
		slog.Error("credlifecycle: cannot determine retained authority; API keys not swept",
			"user_id", userID, "reason", reason)
		return Outcome{Incomplete: true}
	}
	retained, err := s.orgs.GetUserCombinedScopes(ctx, userID)
	if err != nil {
		slog.Error("credlifecycle: failed to re-derive retained scopes for revocation",
			"user_id", userID, "reason", reason, "error", err)
		return Outcome{Incomplete: true}
	}
	// Platform-wide: a TSM key's organization_id is the default organization,
	// not an authority binding, so the owner is the axis (see the package doc).
	keys, err := s.apiKeys.ListAPIKeysByUser(ctx, userID, idstore.OrgScopeAllOrganizations())
	if err != nil {
		slog.Error("credlifecycle: failed to list API keys for revocation",
			"user_id", userID, "reason", reason, "error", err)
		return Outcome{Incomplete: true}
	}
	var out Outcome
	for _, k := range keys {
		if AuthorityRetained(k.Scopes, retained) {
			out.KeysRetained++
			continue
		}
		if err := s.apiKeys.RevokeAPIKey(ctx, k.ID, idstore.OrgScopeAllOrganizations()); err != nil {
			if alreadyGone(err) {
				slog.Info("credlifecycle: over-asking API key already gone",
					"api_key_id", k.ID, "user_id", userID, "reason", reason)
				continue
			}
			slog.Error("credlifecycle: failed to revoke over-asking API key",
				"api_key_id", k.ID, "user_id", userID, "reason", reason, "error", err)
			out.Incomplete = true
			continue
		}
		out.KeysRevoked++
		slog.Info("credlifecycle: API key revoked",
			"api_key_id", k.ID, "user_id", userID, "reason", reason)
	}
	return out
}
