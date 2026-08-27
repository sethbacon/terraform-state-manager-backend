package auditretention

// sweeper_test.go covers the two properties that matter more than the deletion
// itself: the sweep REFUSES to run where holds cannot be honoured, and it
// always passes the exemption when it does run (#373).

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	idstore "github.com/sethbacon/terraform-suite-identity/identity/store"
)

type fakeRepo struct {
	calls   int
	opts    [][]idstore.AuditSweepOption
	cutoffs []time.Time
	batches []int
	// returns is consumed one per call; the last value repeats.
	returns []int64
	err     error
}

func (f *fakeRepo) DeleteAuditLogsBefore(_ context.Context, cutoff time.Time, batchSize int, opts ...idstore.AuditSweepOption) (int64, error) {
	f.calls++
	f.opts = append(f.opts, opts)
	f.cutoffs = append(f.cutoffs, cutoff)
	f.batches = append(f.batches, batchSize)
	if f.err != nil {
		return 0, f.err
	}
	if len(f.returns) == 0 {
		return 0, nil
	}
	if f.calls <= len(f.returns) {
		return f.returns[f.calls-1], nil
	}
	return f.returns[len(f.returns)-1], nil
}

func goodConfig() Config {
	return Config{RetentionDays: 365, Interval: time.Hour, BatchSize: 100, MaxBatches: 5}
}

func okVerify(context.Context, string) error { return nil }

// TestRefusesToSweepWhenHoldsCannotBeHonoured is THE property.
//
// A sweep that runs where the hold table is unreachable deletes evidence
// somebody chose to preserve, and reports success doing it. Fail closed: no
// sweeper at all is the right answer, because an unbounded table is a problem
// and an unbounded table plus silent evidence loss is a worse one.
func TestRefusesToSweepWhenHoldsCannotBeHonoured(t *testing.T) {
	boom := errors.New("relation \"public.legal_holds\" does not exist")
	s, err := New(context.Background(), &fakeRepo{},
		func(context.Context, string) error { return boom }, "public.legal_holds", goodConfig())

	if err == nil {
		t.Fatal("a sweeper was built despite the hold table being unreadable.\n" +
			"It would delete rows no hold could exempt, and report success.")
	}
	if s != nil {
		t.Error("a non-nil sweeper was returned alongside the error")
	}
	if !errors.Is(err, boom) {
		t.Errorf("the underlying cause was lost: %v", err)
	}
	// The message must say what the consequence would have been, not just that
	// a query failed -- an operator reading it decides whether to fix the table
	// or accept an unbounded one.
	if !strings.Contains(err.Error(), "evidence") {
		t.Errorf("the refusal does not explain the consequence: %v", err)
	}
}

// TestVerificationIsMandatory. A nil verifier would let a caller skip the check
// by omission, which is how a fail-closed design quietly becomes fail-open.
func TestVerificationIsMandatory(t *testing.T) {
	if _, err := New(context.Background(), &fakeRepo{}, nil, "public.legal_holds", goodConfig()); err == nil {
		t.Error("a sweeper was built with no verification at all")
	}
}

// TestEverySweepCarriesTheHoldExemption.
//
// The exemption is the whole feature. A sweep that ran without it would delete
// held rows and look identical from the outside -- same log line, same count.
func TestEverySweepCarriesTheHoldExemption(t *testing.T) {
	repo := &fakeRepo{returns: []int64{5, 0}}
	s, err := New(context.Background(), repo, okVerify, "public.legal_holds", goodConfig())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s.sweep()

	if repo.calls == 0 {
		t.Fatal("the sweep issued no delete at all")
	}
	for i, opts := range repo.opts {
		if len(opts) == 0 {
			t.Errorf("batch %d ran with NO options, so no legal-hold exemption was applied", i)
		}
	}
}

// TestRetentionDaysIsRefusedWhenNotPositive.
//
// A zero or negative value puts the cutoff at or AFTER now, which deletes every
// entry no hold covers. The negative case is the dangerous one and the easiest
// to typo.
func TestRetentionDaysIsRefusedWhenNotPositive(t *testing.T) {
	for _, days := range []int{0, -1, -365} {
		cfg := goodConfig()
		cfg.RetentionDays = days
		if _, err := New(context.Background(), &fakeRepo{}, okVerify, "t", cfg); err == nil {
			t.Errorf("retention_days=%d was accepted; the cutoff would be at or after now and the "+
				"sweep would delete everything unheld", days)
		}
	}
}

// TestCutoffIsInThePast pins the arithmetic the test above protects.
func TestCutoffIsInThePast(t *testing.T) {
	repo := &fakeRepo{returns: []int64{0}}
	cfg := goodConfig()
	cfg.RetentionDays = 30
	s, err := New(context.Background(), repo, okVerify, "t", cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	fixed := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return fixed }
	s.sweep()

	if len(repo.cutoffs) == 0 {
		t.Fatal("no sweep ran")
	}
	want := fixed.AddDate(0, 0, -30)
	if !repo.cutoffs[0].Equal(want) {
		t.Errorf("cutoff = %s, want %s (%d days before now)", repo.cutoffs[0], want, cfg.RetentionDays)
	}
	if !repo.cutoffs[0].Before(fixed) {
		t.Error("the cutoff is not in the past, so the sweep would delete current entries")
	}
}

// TestSweepStopsAtMaxBatches.
//
// Termination must not rest solely on "deleted == 0" -- that rests in turn on
// the shared module keeping its exemption inside the LIMIT subselect, and a
// sweep whose liveness depends on another repository's SQL is one refactor from
// looping for ever.
func TestSweepStopsAtMaxBatches(t *testing.T) {
	repo := &fakeRepo{returns: []int64{100}} // never drains
	cfg := goodConfig()
	cfg.MaxBatches = 3
	s, err := New(context.Background(), repo, okVerify, "t", cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s.sweep()

	if repo.calls != 3 {
		t.Errorf("issued %d batches, want exactly %d. A sweep that never drains must stop at its "+
			"ceiling rather than loop.", repo.calls, cfg.MaxBatches)
	}
}

// TestSweepStopsWhenDrained is the ordinary path.
func TestSweepStopsWhenDrained(t *testing.T) {
	repo := &fakeRepo{returns: []int64{10, 10, 0, 99}}
	s, err := New(context.Background(), repo, okVerify, "t", goodConfig())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s.sweep()
	if repo.calls != 3 {
		t.Errorf("issued %d batches, want 3 (two full, one empty). A sweep that keeps going after "+
			"an empty batch would delete rows written since it started.", repo.calls)
	}
}

// TestAFailedBatchStopsTheRun. Continuing after an error would keep hammering a
// failing database, and the count reported afterwards would be wrong.
func TestAFailedBatchStopsTheRun(t *testing.T) {
	repo := &fakeRepo{err: errors.New("connection reset")}
	s, err := New(context.Background(), repo, okVerify, "t", goodConfig())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s.sweep()
	if repo.calls != 1 {
		t.Errorf("issued %d batches after a failure, want 1", repo.calls)
	}
}

// TestBoundsAreRefusedWhenNotPositive.
func TestBoundsAreRefusedWhenNotPositive(t *testing.T) {
	for name, mut := range map[string]func(*Config){
		"interval":    func(c *Config) { c.Interval = 0 },
		"batch_size":  func(c *Config) { c.BatchSize = 0 },
		"max_batches": func(c *Config) { c.MaxBatches = 0 },
	} {
		cfg := goodConfig()
		mut(&cfg)
		if _, err := New(context.Background(), &fakeRepo{}, okVerify, "t", cfg); err == nil {
			t.Errorf("a non-positive %s was accepted", name)
		}
	}
}
