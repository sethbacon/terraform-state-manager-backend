package auth

import (
	"context"
	"sync"
	"time"
)

// SessionState holds the per-login OAuth CSRF state created at the login redirect
// and consumed at the callback.
type SessionState struct {
	State        string
	CreatedAt    time.Time
	ProviderType string
	// RequestID is the SAML AuthnRequest ID for an SP-initiated SAML login. It is
	// passed back to ParseResponse so the IdP response's InResponseTo is validated
	// against the request we issued (defeats unsolicited-response replay). Empty
	// for non-SAML flows.
	RequestID string
	// Nonce and CodeVerifier are the two per-login bindings BeginAuth generates
	// (oidc provider only), carried here between the login redirect and the
	// callback. The nonce binds the returned ID token to this specific login,
	// defeating injection or replay of a token issued for a different attempt;
	// the PKCE verifier proves possession of the original authorization request,
	// defeating redemption of a stolen authorization code elsewhere. Both are
	// empty for non-OIDC flows.
	//
	// Since identity v0.25.0 they arrive as one oidc.CallbackSession
	// (challenge.Session) and go back as one to ExchangeAndVerify, which
	// applies both itself and REFUSES before any network call if either is
	// empty. They stay two fields here because they are two persisted columns
	// (see repositories.LoginStateRepository); the pair is reassembled at the
	// callback rather than split there.
	Nonce        string
	CodeVerifier string
}

// StateStore persists OAuth state tokens between the login redirect and callback.
type StateStore interface {
	Save(ctx context.Context, key string, state *SessionState, ttl time.Duration) error
	Load(ctx context.Context, key string) (*SessionState, error)
	Delete(ctx context.Context, key string) error
}

// MemoryStateStore is an in-memory StateStore suitable for single-instance/dev
// deployments. State entries are single-use (consumed on Load) and expire by TTL.
type MemoryStateStore struct {
	mu     sync.Mutex
	states map[string]stateEntry
}

type stateEntry struct {
	state     *SessionState
	expiresAt time.Time
}

// memoryStateCap bounds the state map. /auth/login is unauthenticated and each
// call inserts an entry, so without a cap abandoned or scripted logins grow
// the map without bound. At the cap the entry closest to expiry is evicted —
// a flood degrades other in-flight logins rather than exhausting memory.
const memoryStateCap = 4096

// NewMemoryStateStore creates an empty in-memory state store.
func NewMemoryStateStore() *MemoryStateStore {
	return &MemoryStateStore{states: make(map[string]stateEntry)}
}

func (m *MemoryStateStore) Save(_ context.Context, key string, state *SessionState, ttl time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	// Opportunistic purge: expired entries are never Loaded (abandoned
	// logins), so this is the only place they are reclaimed.
	for k, e := range m.states {
		if now.After(e.expiresAt) {
			delete(m.states, k)
		}
	}
	if len(m.states) >= memoryStateCap {
		var oldestKey string
		var oldest time.Time
		for k, e := range m.states {
			if oldestKey == "" || e.expiresAt.Before(oldest) {
				oldestKey, oldest = k, e.expiresAt
			}
		}
		delete(m.states, oldestKey)
	}
	m.states[key] = stateEntry{state: state, expiresAt: now.Add(ttl)}
	return nil
}

func (m *MemoryStateStore) Load(_ context.Context, key string) (*SessionState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.states[key]
	if !ok {
		return nil, nil
	}
	delete(m.states, key) // single-use
	if time.Now().After(e.expiresAt) {
		return nil, nil
	}
	return e.state, nil
}

func (m *MemoryStateStore) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.states, key)
	return nil
}
