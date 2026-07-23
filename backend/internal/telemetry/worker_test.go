package telemetry

import (
	"testing"
	"time"
)

func TestWorkerLiveness(t *testing.T) {
	RegisterWorker("w-test", time.Minute)
	t.Cleanup(func() { UnregisterWorker("w-test") })

	// Fresh registration counts as a tick: not stale now, nor just inside the
	// budget (3 intervals with a 2-minute floor -> 3m here... floor is 3m).
	if s := StaleWorkers(time.Now()); len(s) != 0 {
		t.Errorf("fresh worker reported stale: %v", s)
	}
	if s := StaleWorkers(time.Now().Add(2 * time.Minute)); len(s) != 0 {
		t.Errorf("within budget reported stale: %v", s)
	}
	// Past 3x interval it is stale.
	if s := StaleWorkers(time.Now().Add(4 * time.Minute)); len(s) != 1 || s[0] != "w-test" {
		t.Errorf("expected [w-test] stale, got %v", s)
	}
	// A tick resets the budget. Check just inside the 3-minute budget — at the
	// exact boundary, the wall-clock time between the tick and this call tips
	// the comparison on a slow runner.
	WorkerTick("w-test")
	if s := StaleWorkers(time.Now().Add(3*time.Minute - 10*time.Second)); len(s) != 0 {
		t.Errorf("ticked worker reported stale: %v", s)
	}
}

func TestWorkerLiveness_ShortIntervalFloor(t *testing.T) {
	// A 10s-interval worker gets the 2-minute floor, so one slow cycle (30s
	// late) cannot flap readiness.
	RegisterWorker("w-fast", 10*time.Second)
	t.Cleanup(func() { UnregisterWorker("w-fast") })
	if s := StaleWorkers(time.Now().Add(90 * time.Second)); len(s) != 0 {
		t.Errorf("within floor reported stale: %v", s)
	}
	if s := StaleWorkers(time.Now().Add(3 * time.Minute)); len(s) != 1 {
		t.Errorf("past floor should be stale, got %v", s)
	}
}

func TestWorkerTick_UnregisteredIgnored(t *testing.T) {
	WorkerTick("never-registered") // must not panic or create an entry
	if s := StaleWorkers(time.Now().Add(24 * time.Hour)); len(s) != 0 {
		t.Errorf("unregistered tick created a tracked worker: %v", s)
	}
}
