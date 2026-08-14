package api

import (
	"database/sql"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"

	"github.com/terraform-state-manager/terraform-state-manager/internal/config"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
	"github.com/terraform-state-manager/terraform-state-manager/internal/platformadmin"
	"github.com/terraform-state-manager/terraform-state-manager/internal/services/notify"
	"github.com/terraform-state-manager/terraform-state-manager/internal/services/statesync"
)

// newWorkerTestDeps builds a backgroundWorkerDeps over a sqlmock DB. Intervals
// are set large so any started loop parks on its first tick rather than querying
// during the test. Extracting newBackgroundWorkers is what makes this reachable
// at all — the former inline block could only run inside NewRouter (#265).
func newWorkerTestDeps(t *testing.T, db *sql.DB, enabled bool) backgroundWorkerDeps {
	t.Helper()
	t.Setenv("TSM_ENCRYPTION_KEY", "0123456789abcdef0123456789abcdef")
	cfg := &config.Config{}
	cfg.Workers.Enabled = enabled
	cfg.Drift.RunTTL = time.Hour
	cfg.Drift.ReconcileInterval = time.Hour
	cfg.Notifications.APIKeyExpiryCheckIntervalHours = 24
	drift := NewDriftHandlers(cfg, db, nil, nil)
	return backgroundWorkerDeps{
		cfg:        cfg,
		database:   db,
		identityDB: db,
		sources:    NewSourcesHandlers(db, nil),
		drift:      drift,
		health:     NewHealthHandlers(cfg, db, nil, nil),
		driftDisp:  driftDispatcher{drift: drift},
		smtpCfg:    &notify.SMTPConfig{},
	}
}

// TestNewBackgroundWorkers_NilDBIsNoop covers the guard that lets unit tests
// build the router with a nil DB without spinning up goroutines.
func TestNewBackgroundWorkers_NilDBIsNoop(t *testing.T) {
	stop := newBackgroundWorkers(backgroundWorkerDeps{cfg: &config.Config{}, database: nil})
	if stop == nil {
		t.Fatal("stop must never be nil")
	}
	stop() // must be a safe no-op
}

// TestNewBackgroundWorkers_Disabled attaches the always-on syncer but starts no
// leader election / periodic loops when workers are disabled on this replica.
func TestNewBackgroundWorkers_Disabled(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	d := newWorkerTestDeps(t, db, false)
	stop := newBackgroundWorkers(d)
	if stop == nil {
		t.Fatal("stop must never be nil")
	}
	if d.sources.syncer == nil {
		t.Error("the state syncer must be attached on every replica, even with workers disabled")
	}
	stop() // no elector started -> safe no-op
}

// TestNewBackgroundWorkers_EnabledStartsAndStopsElector exercises the leader-
// election wiring: with workers enabled the elector campaigns in a goroutine and
// the returned stop tears it down cleanly (no hang), which the previous inline
// block could not be tested for.
func TestNewBackgroundWorkers_EnabledStartsAndStopsElector(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	d := newWorkerTestDeps(t, db, true)
	stop := newBackgroundWorkers(d)
	if stop == nil {
		t.Fatal("stop must never be nil")
	}
	if d.sources.syncer == nil {
		t.Error("the state syncer must be attached before leader election starts")
	}

	done := make(chan struct{})
	go func() { stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("stop() did not return; elector teardown hangs")
	}
}

// TestStartWorkers_ConstructsAndStops directly drives the worker-startup body
// (schedule runner, state-sync, drift/health reconcilers, API-key-expiry
// notifier) that previously only ran on a production leader. It must construct,
// start, and tear the whole set down without panicking or hanging.
func TestStartWorkers_ConstructsAndStops(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	d := newWorkerTestDeps(t, db, true)
	syncer := statesync.New(
		repositories.NewSourceRepository(db),
		repositories.NewStateAnalysisRepository(db),
		ConnectSource,
	)

	stop := d.startWorkers(syncer)
	if stop == nil {
		t.Fatal("startWorkers must return a non-nil stop")
	}

	done := make(chan struct{})
	go func() { stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("startWorkers stop() did not return; a worker loop teardown hangs")
	}
}

// TestNewBackgroundWorkers_StartsTheAuditRelayEvenWithWorkersDisabled.
//
// Every other periodic loop here is leader-gated, and the audit relay
// deliberately is not. It claims with FOR UPDATE SKIP LOCKED, so several
// replicas take disjoint batches rather than colliding — the property leader
// election exists to provide is one this job already has. Gating it would tie
// the audit trail's liveness to leadership: TSM's documented shape runs workers
// on exactly ONE replica, so a leader-gated relay would stop delivering
// platform-admin audit records the moment that replica went down, while every
// other replica reported healthy and the intents piled up undelivered.
//
// Observed rather than assumed: the relay's first cycle is scripted on its own
// handle, and the test waits for those expectations to be MET.
func TestNewBackgroundWorkers_StartsTheAuditRelayEvenWithWorkersDisabled(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()
	relayDB, relayMock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New (relay): %v", err)
	}
	defer relayDB.Close()

	svc, err := platformadmin.New(relayDB, relayDB)
	if err != nil {
		t.Fatalf("platformadmin.New: %v", err)
	}

	// One full cycle against an empty outbox: claim, commit, report the backlog,
	// prune delivered history.
	relayMock.ExpectBegin()
	relayMock.ExpectQuery(`FROM "audit_outbox"`).
		WillReturnRows(sqlmock.NewRows([]string{"event_id", "occurred_at", "action", "actor_user_id",
			"actor_email", "organization_id", "resource_type", "resource_id", "ip_address", "metadata", "attempts"}))
	relayMock.ExpectCommit()
	relayMock.ExpectQuery(`FROM "audit_outbox"`).
		WillReturnRows(sqlmock.NewRows([]string{"count", "failed", "oldest"}).AddRow(0, 0, nil))
	relayMock.ExpectExec(`DELETE FROM "audit_outbox"`).WillReturnResult(sqlmock.NewResult(0, 0))

	d := newWorkerTestDeps(t, db, false) // workers DISABLED on this replica
	d.auditRelay = svc.Relay()
	stop := newBackgroundWorkers(d)
	t.Cleanup(stop)

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if err := relayMock.ExpectationsWereMet(); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("the audit outbox relay never ran a cycle on a workers-disabled replica: %v",
		relayMock.ExpectationsWereMet())
}
