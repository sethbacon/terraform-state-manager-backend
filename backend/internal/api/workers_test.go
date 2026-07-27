package api

import (
	"database/sql"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"

	"github.com/terraform-state-manager/terraform-state-manager/internal/config"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
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
