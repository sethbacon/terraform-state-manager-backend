// Package auditretention bounds audit_logs, without deleting evidence somebody
// decided to keep (#373).
//
// # Two properties, and the order they are checked in
//
// The sweep exempts any audit row covered by an ACTIVE legal hold. The
// exemption is rendered by the shared module and applied INSIDE the batch
// selection, so held rows are never selected -- a filter applied after LIMIT
// would let a batch fill with held rows, delete none, and hand the loop the
// same batch for ever.
//
// That exemption is only real if the hold table is reachable ON THE SWEEP'S OWN
// CONNECTION. audit_logs is read through the identity pool, and a hold table
// created on the app pool -- or in a deployment that splits the two databases --
// is invisible to the sweep: every hold placed, every UI confirming it, and
// every sweep deleting the rows anyway.
//
// So the sweeper REFUSES TO START unless that is verified. Fail closed: a
// deployment whose holds cannot be honoured does not get a sweep at all. An
// unbounded table is a problem; an unbounded table plus silent evidence loss is
// a worse one.
//
// # Why it is bounded twice
//
// batch_size bounds one DELETE; max_batches bounds one RUN. Termination must
// not rest solely on "deleted == 0", because that rests in turn on the shared
// module keeping its exemption inside the LIMIT subselect. It does today, and a
// sweep whose liveness depends on a detail of another repository's SQL is one
// refactor away from looping for ever.
package auditretention

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	idstore "github.com/sethbacon/terraform-suite-identity/identity/store"
)

// sweepTimeout bounds one run. Generous, because a first sweep on a long-lived
// deployment may page through a lot of history, and short enough that a wedged
// database does not hold the worker for ever.
const sweepTimeout = 30 * time.Minute

// Config is the operator-facing shape.
type Config struct {
	RetentionDays int
	Interval      time.Duration
	BatchSize     int
	MaxBatches    int
}

// auditSweeper is the part of idstore.AuditRepository this uses. An interface so
// the loop is testable without a database -- the sweep's arithmetic and its
// stopping conditions are the parts worth testing, and they do not need SQL.
type auditSweeper interface {
	DeleteAuditLogsBefore(ctx context.Context, cutoff time.Time, batchSize int, opts ...idstore.AuditSweepOption) (int64, error)
}

// Sweeper deletes audit entries past the retention period, except those under
// an active legal hold.
type Sweeper struct {
	repo     auditSweeper
	cfg      Config
	holdOpts []idstore.AuditSweepOption
	now      func() time.Time
	stopCh   chan struct{}
	logger   *slog.Logger
}

// New builds a Sweeper, having PROVED the hold table is readable on the same
// connection the sweep will run on.
//
// verifyDB must be the handle idstore.AuditRepository was built with. Passing a
// different one would verify a table the sweep never reads, which is the exact
// failure this check exists to prevent -- so the argument is the connection, not
// a boolean somebody computed earlier.
func New(ctx context.Context, repo auditSweeper, verify func(context.Context, string) error, holdTable string, cfg Config) (*Sweeper, error) {
	if repo == nil {
		return nil, errors.New("auditretention: no audit repository")
	}
	if cfg.RetentionDays <= 0 {
		// Refused rather than defaulted. A zero or negative retention puts the
		// cutoff at or after now, which deletes every entry no hold covers.
		return nil, fmt.Errorf("auditretention: retention_days must be > 0, got %d", cfg.RetentionDays)
	}
	if cfg.Interval <= 0 || cfg.BatchSize <= 0 || cfg.MaxBatches <= 0 {
		return nil, fmt.Errorf("auditretention: interval, batch_size and max_batches must all be > 0 "+
			"(got %s, %d, %d)", cfg.Interval, cfg.BatchSize, cfg.MaxBatches)
	}
	if verify == nil {
		return nil, errors.New("auditretention: no legal-hold verification supplied; the sweep " +
			"must not run where holds cannot be honoured")
	}
	if err := verify(ctx, holdTable); err != nil {
		return nil, fmt.Errorf("auditretention: refusing to sweep -- the legal-hold table %q is not "+
			"readable on the connection the sweep runs on, so no hold could be honoured and this "+
			"sweep would delete evidence somebody chose to preserve: %w", holdTable, err)
	}
	return &Sweeper{
		repo:     repo,
		cfg:      cfg,
		holdOpts: []idstore.AuditSweepOption{idstore.WithLegalHolds(holdTable)},
		now:      time.Now,
		stopCh:   make(chan struct{}),
		logger:   slog.With("component", "auditretention"),
	}, nil
}

// Start launches the loop. The first sweep is deferred by one interval rather
// than run immediately: an operator who has just enabled this deserves the
// chance to see the startup log and stop the process before anything is
// deleted, which a boot-time sweep would not give them.
func (s *Sweeper) Start() {
	ticker := time.NewTicker(s.cfg.Interval)
	s.logger.Info("audit retention started",
		"retention_days", s.cfg.RetentionDays,
		"interval", s.cfg.Interval.String(),
		"first_sweep_in", s.cfg.Interval.String())
	go func() {
		for {
			select {
			case <-ticker.C:
				s.sweep()
			case <-s.stopCh:
				ticker.Stop()
				s.logger.Info("audit retention stopped")
				return
			}
		}
	}()
}

// Stop ends the loop. Safe to call once.
func (s *Sweeper) Stop() { close(s.stopCh) }

// sweep runs one bounded pass.
func (s *Sweeper) sweep() {
	ctx, cancel := context.WithTimeout(context.Background(), sweepTimeout)
	defer cancel()

	cutoff := s.now().UTC().AddDate(0, 0, -s.cfg.RetentionDays)
	total := int64(0)
	for batch := 0; batch < s.cfg.MaxBatches; batch++ {
		n, err := s.repo.DeleteAuditLogsBefore(ctx, cutoff, s.cfg.BatchSize, s.holdOpts...)
		if err != nil {
			s.logger.Error("audit retention sweep failed",
				"error", err, "cutoff", cutoff, "deleted_before_failure", total)
			return
		}
		total += n
		if n == 0 {
			break
		}
		if batch == s.cfg.MaxBatches-1 {
			// Reported, because the alternative is a sweep that quietly never
			// catches up. An operator seeing this repeatedly needs a larger
			// batch or a shorter interval, and cannot know that from silence.
			s.logger.Warn("audit retention hit its batch ceiling; more rows remain past the cutoff",
				"max_batches", s.cfg.MaxBatches, "batch_size", s.cfg.BatchSize, "deleted", total)
		}
	}
	if total > 0 {
		s.logger.Info("audit retention swept", "deleted", total, "cutoff", cutoff)
	}
}
