package statesource

import (
	"bytes"
	"strings"
	"testing"
)

func TestReadCappedTo(t *testing.T) {
	// Under the limit: returned in full.
	body := []byte("small state body")
	got, err := readCappedTo(bytes.NewReader(body), 1024)
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("under limit: err=%v got=%q", err, got)
	}

	// Exactly at the limit: allowed.
	exact := bytes.Repeat([]byte("a"), 16)
	if got, err := readCappedTo(bytes.NewReader(exact), 16); err != nil || len(got) != 16 {
		t.Errorf("exactly at limit should be allowed: err=%v len=%d", err, len(got))
	}

	// Over the limit: rejected, not silently truncated.
	over := strings.NewReader(strings.Repeat("a", 17))
	if _, err := readCappedTo(over, 16); err == nil {
		t.Error("over limit must return an error, not truncate")
	}
}

// TestReadCappedUsesMaxStateBytes confirms the public helper enforces the 64 MiB
// package cap (a small body passes; the exhaustive over-cap case is covered by
// TestReadCappedTo with a small limit to avoid a 64 MiB allocation).
func TestReadCappedUsesMaxStateBytes(t *testing.T) {
	if maxStateBytes != 64<<20 {
		t.Fatalf("maxStateBytes = %d, want 64 MiB", maxStateBytes)
	}
	got, err := readCapped(bytes.NewReader([]byte("ok")))
	if err != nil || string(got) != "ok" {
		t.Errorf("readCapped small body: err=%v got=%q", err, got)
	}
}
