-- Per-user token-revocation watermarks (issue #330).
--
-- revoked_tokens (the identity schema's JTI denylist) can only retire a token
-- whose JTI is known — logout and single-token admin revocation. An event that
-- REDUCES a principal's derived authority (organization membership removed,
-- role template reassigned, user deprovisioned via SCIM or an IdP group sync)
-- has to retire EVERY outstanding session for that user, and their JTIs are
-- not tracked anywhere. Instead a watermark is upserted per user: any JWT whose
-- iat predates the watermark is treated as revoked by the auth middleware.
--
-- No FK to users: identity data may live in a separate identity database
-- (TSM_IDENTITY_DATABASE_*), while this table always lives on the app's own
-- connection, so a cross-database reference is not expressible.
CREATE TABLE IF NOT EXISTS user_token_revocations (
    user_id        UUID PRIMARY KEY,
    revoked_before TIMESTAMPTZ NOT NULL,
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
