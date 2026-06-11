package auth

import (
	"context"
	"testing"
	"time"
)

func TestMemoryStateStore_ImplementsInterface(t *testing.T) {
	var _ StateStore = NewMemoryStateStore()
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
