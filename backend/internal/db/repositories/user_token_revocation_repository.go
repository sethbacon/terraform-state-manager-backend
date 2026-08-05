// user_token_revocation_repository.go implements per-user token-revocation
// watermarks (issue #330).
//
// The identity schema's JTI denylist (idstore.TokenRepository /
// revoked_tokens) can only revoke tokens whose JTI is known — logout and
// single-token admin revocation. An authority REDUCTION (a member removed from
// an organization, a member's role template reassigned, a user deprovisioned by
// SCIM or an IdP group sync) has to retire every outstanding session for the
// affected user, whose JTIs this app never records. This repository stores one
// watermark per user instead: any JWT whose iat predates the watermark is
// treated as revoked by the auth middleware.
//
// The table lives on the app's own connection rather than the identity one, so
// it works unchanged whether identity data shares the app database (the
// default) or lives in a separate one (TSM_IDENTITY_DATABASE_*).
package repositories

import (
	"context"
	"database/sql"
	"time"
)

// UserTokenRevocationRepository manages per-user token-revocation watermarks.
type UserTokenRevocationRepository struct {
	db *sql.DB
}

// NewUserTokenRevocationRepository creates the DAO over the app connection.
func NewUserTokenRevocationRepository(db *sql.DB) *UserTokenRevocationRepository {
	return &UserTokenRevocationRepository{db: db}
}

// RevokeAllUserTokens invalidates every JWT issued to the user before now by
// moving the user's revocation watermark to the current time. Tokens issued
// after this call (e.g. a fresh login) validate normally.
func (r *UserTokenRevocationRepository) RevokeAllUserTokens(ctx context.Context, userID string) error {
	query := `
		INSERT INTO user_token_revocations (user_id, revoked_before, updated_at)
		VALUES ($1, NOW(), NOW())
		ON CONFLICT (user_id) DO UPDATE
		SET revoked_before = EXCLUDED.revoked_before, updated_at = EXCLUDED.updated_at
	`
	_, err := r.db.ExecContext(ctx, query, userID)
	return err
}

// TokensRevokedSince reports whether tokens issued to the user at issuedAt are
// revoked, i.e. whether the user's watermark postdates the token's iat claim.
//
// issuedAt carries only whole-second precision (golang-jwt's NumericDate floors
// JWT iat/exp to the second per RFC 7519) while revoked_before is a
// full-precision Postgres timestamp, so a token minted and a revocation landing
// within the same wall-clock second are ambiguous: the floored iat alone cannot
// say whether the real mint time was before or after the revocation. This is
// deliberately NOT "fixed" by truncating revoked_before to match iat's
// precision — that would only move the ambiguity to the unsafe side, where a
// token minted just before a revocation, in the same second, reads as valid.
// The plain `>` below always resolves the ambiguous window toward "revoked": a
// false positive costs a fresh login one extra round trip, the reverse would be
// a real session surviving a revocation it should not have.
func (r *UserTokenRevocationRepository) TokensRevokedSince(ctx context.Context, userID string, issuedAt time.Time) (bool, error) {
	query := `SELECT EXISTS(SELECT 1 FROM user_token_revocations WHERE user_id = $1 AND revoked_before > $2)`
	var revoked bool
	err := r.db.QueryRowContext(ctx, query, userID, issuedAt).Scan(&revoked)
	return revoked, err
}

// CleanupExpiredWatermarks removes watermarks old enough that every token they
// could revoke has already expired naturally. maxTokenTTL must be at least the
// longest JWT lifetime this app issues (api.sessionTTL, 24h); a watermark older
// than that can no longer match any structurally valid token.
func (r *UserTokenRevocationRepository) CleanupExpiredWatermarks(ctx context.Context, maxTokenTTL time.Duration) error {
	query := `DELETE FROM user_token_revocations WHERE revoked_before < $1`
	_, err := r.db.ExecContext(ctx, query, time.Now().Add(-maxTokenTTL))
	return err
}
