package scheduler

import (
	"testing"
	"time"
)

func TestComputeNextRun_Cron(t *testing.T) {
	from := time.Date(2026, 6, 9, 10, 30, 0, 0, time.UTC)
	// Daily at 03:00 — next occurrence is the following day at 03:00.
	got := ComputeNextRun("0 3 * * *", from)
	if got == nil {
		t.Fatal("expected a next run for a valid cron expression")
	}
	want := time.Date(2026, 6, 10, 3, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("next = %v, want %v", got, want)
	}
}

func TestComputeNextRun_Keywords(t *testing.T) {
	from := time.Date(2026, 6, 9, 10, 0, 0, 0, time.UTC)
	cases := map[string]time.Duration{
		"daily":     24 * time.Hour,
		"weekly":    7 * 24 * time.Hour,
		"every 15m": 15 * time.Minute,
		"every 2h":  2 * time.Hour,
		"every 10s": time.Minute, // sub-minute is clamped up to 1 minute
	}
	for expr, delta := range cases {
		got := ComputeNextRun(expr, from)
		if got == nil {
			t.Fatalf("%q: expected a next run", expr)
		}
		if want := from.Add(delta); !got.Equal(want) {
			t.Errorf("%q: next = %v, want %v", expr, got, want)
		}
	}
}

func TestComputeNextRun_Invalid(t *testing.T) {
	from := time.Now()
	for _, expr := range []string{"", "not-a-cron", "every", "every banana", "hourly"} {
		if got := ComputeNextRun(expr, from); got != nil {
			t.Errorf("ComputeNextRun(%q) = %v, want nil", expr, got)
		}
	}
}
