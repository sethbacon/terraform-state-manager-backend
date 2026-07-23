package repositories

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/terraform-state-manager/terraform-state-manager/internal/auth"
)

// LoginStateRepository is the database-backed auth.StateStore: the single-use
// OAuth state / PKCE verifier / OIDC nonce / SAML AuthnRequest id created at
// the login redirect survives until the callback even when the two requests
// land on different replicas behind a load balancer. Rows are single-use
// (consumed atomically by Load) and expire by TTL, with expired rows reaped
// opportunistically on each Save — no background sweeper, so it needs no
// leader election and works identically on every replica.
type LoginStateRepository struct {
	db *sql.DB
}

// NewLoginStateRepository creates the DAO. The login_states table lives in the
// app (public) schema; the identity connection's search_path resolves it.
func NewLoginStateRepository(db *sql.DB) *LoginStateRepository {
	return &LoginStateRepository{db: db}
}

// Save persists the state under key with the given TTL, replacing any previous
// entry for the key. Expired rows fleet-wide are reaped in the same call.
func (r *LoginStateRepository) Save(ctx context.Context, key string, state *auth.SessionState, ttl time.Duration) error {
	// Opportunistic reap bounds the table: /auth/login is unauthenticated, so
	// abandoned or scripted logins otherwise accumulate rows forever. Failure
	// here must not block a login; the next save retries.
	_, _ = r.db.ExecContext(ctx, `DELETE FROM login_states WHERE expires_at < now()`)

	blob, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("failed to encode login state: %w", err)
	}
	_, err = r.db.ExecContext(ctx, `
		INSERT INTO login_states (key, state, expires_at)
		VALUES ($1, $2, now() + make_interval(secs => $3))
		ON CONFLICT (key) DO UPDATE SET state = EXCLUDED.state, expires_at = EXCLUDED.expires_at`,
		key, blob, ttl.Seconds())
	return err
}

// Load consumes the state for key: the row is deleted atomically, so even two
// racing callbacks on different replicas redeem it at most once. A missing or
// expired entry returns (nil, nil), matching the memory store's contract.
func (r *LoginStateRepository) Load(ctx context.Context, key string) (*auth.SessionState, error) {
	var blob []byte
	var expired bool
	err := r.db.QueryRowContext(ctx,
		`DELETE FROM login_states WHERE key = $1 RETURNING state, expires_at < now()`, key).
		Scan(&blob, &expired)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if expired {
		return nil, nil
	}
	var state auth.SessionState
	if err := json.Unmarshal(blob, &state); err != nil {
		return nil, fmt.Errorf("failed to decode login state: %w", err)
	}
	return &state, nil
}

// Delete removes the state for key (idempotent).
func (r *LoginStateRepository) Delete(ctx context.Context, key string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM login_states WHERE key = $1`, key)
	return err
}
