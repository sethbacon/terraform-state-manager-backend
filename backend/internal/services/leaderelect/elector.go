// Package leaderelect elects exactly one worker leader across replicas using a
// session-scoped Postgres advisory lock, so running TSM_WORKERS_ENABLED=true on
// more than one replica is safe by construction instead of by operator
// discipline: every worker-enabled replica participates, one wins and starts
// the background loops, and the others stand by. The lock is tied to the
// holder's database session, so a crashed or partitioned leader releases it
// implicitly (connection teardown) and a standby promotes on its next retry.
//
// Requires session-mode database connections (the default lib/pq pool). A
// transaction-pooling proxy (e.g. pgbouncer in transaction mode) would break
// session advisory locks and must not front this connection.
//
// Like the other services this is a leaf package: it depends only on
// database/sql and is handed an opaque start callback by the router.
package leaderelect

import (
	"context"
	"database/sql"
	"log/slog"
	"time"
)

// lockKey identifies the worker-leader advisory lock. Arbitrary but stable —
// spells "tsmwkrs" — and must not collide with other advisory locks in the
// shared database.
const lockKey int64 = 0x74736d776b7273

const (
	// retryInterval is how often a standby re-attempts the lock, and how often
	// the leader verifies its session is still alive. Failover time is at most
	// one leader-detect interval plus one standby retry (~2x this).
	retryInterval = 15 * time.Second
	// opTimeout bounds each acquire/verify/release round-trip.
	opTimeout = 5 * time.Second
)

// Elector runs the election loop. start is invoked once on winning leadership
// and must return the stop func for everything it started; that stop func is
// invoked when leadership is lost or the elector is stopped.
type Elector struct {
	db       *sql.DB
	start    func() (stop func())
	interval time.Duration
	stopCh   chan struct{}
	doneCh   chan struct{}
	logger   *slog.Logger
}

// New constructs an Elector. Call Start to begin campaigning.
func New(db *sql.DB, start func() (stop func())) *Elector {
	return &Elector{
		db:       db,
		start:    start,
		interval: retryInterval,
		stopCh:   make(chan struct{}),
		doneCh:   make(chan struct{}),
		logger:   slog.With("component", "leaderelect"),
	}
}

// Start launches the campaign loop in a goroutine and returns immediately.
func (e *Elector) Start() {
	e.logger.Info("campaigning for worker leadership", "retry_interval", e.interval.String())
	go e.run()
}

// Stop ends the loop, stopping the workers and releasing the lock when this
// replica is the leader. Blocks until teardown completes. Safe to call once.
func (e *Elector) Stop() {
	close(e.stopCh)
	<-e.doneCh
}

func (e *Elector) run() {
	defer close(e.doneCh)
	for {
		if conn, ok := e.tryAcquire(); ok {
			e.logger.Info("became worker leader")
			stopWorkers := e.start()
			lost := e.hold(conn)
			stopWorkers()
			e.release(conn)
			if !lost {
				return // Stop() was called while leading
			}
			e.logger.Warn("lost worker leadership (database session ended); re-campaigning")
		}
		select {
		case <-time.After(e.interval):
		case <-e.stopCh:
			return
		}
	}
}

// tryAcquire grabs a dedicated session and attempts the advisory lock on it.
// The connection is retained for the whole leadership term — the lock lives
// exactly as long as this session does.
func (e *Elector) tryAcquire() (*sql.Conn, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	conn, err := e.db.Conn(ctx)
	if err != nil {
		e.logger.Error("failed to open election connection", "error", err)
		return nil, false
	}
	var got bool
	if err := conn.QueryRowContext(ctx, "SELECT pg_try_advisory_lock($1)", lockKey).Scan(&got); err != nil {
		e.logger.Error("advisory lock attempt failed", "error", err)
		_ = conn.Close()
		return nil, false
	}
	if !got {
		_ = conn.Close()
		return nil, false
	}
	return conn, true
}

// hold blocks while leadership lasts: it pings the lock's session every
// interval and returns true when the session died (lock implicitly released
// server-side — step down), or false when Stop was requested.
func (e *Elector) hold(conn *sql.Conn) (lost bool) {
	t := time.NewTicker(e.interval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
			err := conn.PingContext(ctx)
			cancel()
			if err != nil {
				return true
			}
		case <-e.stopCh:
			return false
		}
	}
}

// release unlocks and closes the leadership session (best-effort — a dead
// session already released the lock server-side).
func (e *Elector) release(conn *sql.Conn) {
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	var unlocked bool
	_ = conn.QueryRowContext(ctx, "SELECT pg_advisory_unlock($1)", lockKey).Scan(&unlocked)
	_ = conn.Close()
}
