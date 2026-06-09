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

// NewMemoryStateStore creates an empty in-memory state store.
func NewMemoryStateStore() *MemoryStateStore {
	return &MemoryStateStore{states: make(map[string]stateEntry)}
}

func (m *MemoryStateStore) Save(_ context.Context, key string, state *SessionState, ttl time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.states[key] = stateEntry{state: state, expiresAt: time.Now().Add(ttl)}
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
