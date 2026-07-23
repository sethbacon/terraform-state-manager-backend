package auth

import (
	"context"
	"strconv"
	"testing"
	"time"
)

func TestMemoryStateStore_ImplementsInterface(t *testing.T) {
	var _ StateStore = NewMemoryStateStore()
}

// TestMemoryStateStore_CapAndPurge: /auth/login is unauthenticated, so the map
// must stay bounded — expired entries are purged on Save and, at the cap, the
// entry closest to expiry is evicted for the new one.
func TestMemoryStateStore_CapAndPurge(t *testing.T) {
	store := NewMemoryStateStore()
	ctx := context.Background()

	for i := 0; i < memoryStateCap+100; i++ {
		key := "flood-" + strconv.Itoa(i)
		if err := store.Save(ctx, key, &SessionState{State: key}, time.Minute); err != nil {
			t.Fatalf("Save(%s): %v", key, err)
		}
	}
	if n := len(store.states); n > memoryStateCap {
		t.Fatalf("store grew past the cap: %d > %d", n, memoryStateCap)
	}
	// The newest entry survived the eviction churn.
	if got, _ := store.Load(ctx, "flood-"+strconv.Itoa(memoryStateCap+99)); got == nil {
		t.Error("newest entry must survive cap eviction")
	}

	// Expired entries are reclaimed by the next Save even though they are
	// never Loaded (abandoned logins).
	store2 := NewMemoryStateStore()
	_ = store2.Save(ctx, "gone", &SessionState{State: "gone"}, -time.Second)
	_ = store2.Save(ctx, "fresh", &SessionState{State: "fresh"}, time.Minute)
	if _, ok := store2.states["gone"]; ok {
		t.Error("expired entry must be purged on the next Save")
	}
}

func TestMemoryStateStore_SaveAndLoad(t *testing.T) {
	store := NewMemoryStateStore()
	ctx := context.Background()

	state := &SessionState{
		State:        "csrf-token-value",
		CreatedAt:    time.Now(),
		ProviderType: "oidc",
		RequestID:    "saml-req-1",
	}
	if err := store.Save(ctx, "key1", state, time.Minute); err != nil {
		t.Fatalf("Save() error: %v", err)
	}

	got, err := store.Load(ctx, "key1")
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if got == nil {
		t.Fatal("Load() returned nil for a saved key")
	}
	if got.State != "csrf-token-value" || got.ProviderType != "oidc" || got.RequestID != "saml-req-1" {
		t.Errorf("Load() = %+v, fields do not match saved state", got)
	}

	// Single-use: a second Load must miss.
	again, err := store.Load(ctx, "key1")
	if err != nil {
		t.Fatalf("second Load() error: %v", err)
	}
	if again != nil {
		t.Error("Load() returned a state that should have been consumed (single-use)")
	}
}

func TestMemoryStateStore_LoadNonExistent(t *testing.T) {
	store := NewMemoryStateStore()
	got, err := store.Load(context.Background(), "missing")
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if got != nil {
		t.Errorf("Load() of a missing key = %+v, want nil", got)
	}
}

func TestMemoryStateStore_Delete(t *testing.T) {
	store := NewMemoryStateStore()
	ctx := context.Background()

	if err := store.Save(ctx, "key1", &SessionState{State: "v"}, time.Minute); err != nil {
		t.Fatalf("Save() error: %v", err)
	}
	if err := store.Delete(ctx, "key1"); err != nil {
		t.Fatalf("Delete() error: %v", err)
	}
	got, err := store.Load(ctx, "key1")
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if got != nil {
		t.Error("Load() returned a deleted state")
	}
}

func TestMemoryStateStore_DeleteNonExistent(t *testing.T) {
	store := NewMemoryStateStore()
	if err := store.Delete(context.Background(), "missing"); err != nil {
		t.Errorf("Delete() of a missing key returned error: %v", err)
	}
}

func TestMemoryStateStore_TTLExpiry(t *testing.T) {
	store := NewMemoryStateStore()
	ctx := context.Background()

	if err := store.Save(ctx, "key1", &SessionState{State: "v"}, 10*time.Millisecond); err != nil {
		t.Fatalf("Save() error: %v", err)
	}
	time.Sleep(25 * time.Millisecond)

	got, err := store.Load(ctx, "key1")
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if got != nil {
		t.Error("Load() returned an expired state")
	}
}
