-- Durable single-use login state (OAuth state / PKCE verifier / OIDC nonce /
-- SAML AuthnRequest id). The login redirect and its callback are separate HTTP
-- requests; storing the state in the database lets them land on DIFFERENT
-- replicas behind a load balancer. Rows expire by TTL and are reaped
-- opportunistically on each save, so abandoned logins cannot grow the table
-- without bound.
CREATE TABLE IF NOT EXISTS login_states (
    key        TEXT PRIMARY KEY,
    state      JSONB NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_login_states_expires_at ON login_states (expires_at);
