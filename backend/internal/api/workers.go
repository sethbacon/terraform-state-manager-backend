package api

import (
	"context"
	"database/sql"
	"log/slog"

	identitymailer "github.com/sethbacon/terraform-suite-identity/identity/mailer"
	identitynotify "github.com/sethbacon/terraform-suite-identity/identity/notify"
	idstore "github.com/sethbacon/terraform-suite-identity/identity/store"

	"github.com/terraform-state-manager/terraform-state-manager/internal/config"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
	"github.com/terraform-state-manager/terraform-state-manager/internal/services/driftreconcile"
	"github.com/terraform-state-manager/terraform-state-manager/internal/services/healthreconcile"
	"github.com/terraform-state-manager/terraform-state-manager/internal/services/leaderelect"
	"github.com/terraform-state-manager/terraform-state-manager/internal/services/notify"
	"github.com/terraform-state-manager/terraform-state-manager/internal/services/scheduler"
	"github.com/terraform-state-manager/terraform-state-manager/internal/services/statesync"
)

// backgroundWorkerDeps carries everything the leader-elected background worker
// set needs. NewRouter populates it from its locals; extracting it keeps this
// operationally-critical wiring out of NewRouter's body and — unlike the former
// inline construction — makes it unit-testable with a sqlmock *sql.DB (#265).
type backgroundWorkerDeps struct {
	cfg        *config.Config
	database   *sql.DB
	identityDB *sql.DB
	sources    *SourcesHandlers
	drift      *DriftHandlers
	health     *HealthHandlers
	driftDisp  driftDispatcher
	smtpCfg    *notify.SMTPConfig
}

// newBackgroundWorkers attaches the always-on state syncer to the sources plane
// and, on a worker-enabled replica, starts the Postgres-advisory-lock leader-
// elected periodic worker set. It returns a stop func that tears the set down.
//
// The syncer OBJECT is attached on every replica (post-write refreshes and
// source-create backfills must work everywhere); only the PERIODIC loops are
// leader-gated, so among several worker-enabled replicas a single advisory-lock
// leader runs them and a mis-scaled deployment cannot double-fire schedules,
// syncs, or expiry emails. With a nil database (unit tests) it is a no-op, and
// with workers disabled it attaches the syncer but starts no loops.
func newBackgroundWorkers(d backgroundWorkerDeps) (stop func()) {
	stop = func() {}
	if d.database == nil {
		return stop
	}
	syncer := statesync.New(
		repositories.NewSourceRepository(d.database),
		repositories.NewStateAnalysisRepository(d.database),
		ConnectSource,
	)
	d.sources.AttachSyncer(syncer)
	if !d.cfg.Workers.Enabled {
		slog.Info("background workers disabled on this replica (workers.enabled=false); " +
			"schedule firing and periodic state sync run on a worker-enabled replica")
		return stop
	}
	elector := leaderelect.New(d.database, func() (stopWorkers func()) {
		return d.startWorkers(syncer)
	})
	elector.Start()
	return elector.Stop
}

// startWorkers constructs and starts the periodic worker loops (schedule runner,
// state-sync reconcile, drift/health stuck-run reconcilers, API-key-expiry
// notifier) and returns a stop func that halts them all. It runs once when this
// replica wins leadership; the elector invokes the returned stop on leadership
// loss or shutdown.
func (d backgroundWorkerDeps) startWorkers(syncer *statesync.Syncer) (stop func()) {
	runner := scheduler.New(repositories.NewScheduleRepository(d.database), d.driftDisp)
	runner.Start()
	syncer.Start()
	// Reap drift runs stuck in "dispatched" whose CI job never called back
	// (build/agent died), firing the same failure alert a real callback would.
	reconciler := driftreconcile.New(
		repositories.NewDriftRepository(d.database),
		driftFailureNotifier{drift: d.drift},
		d.cfg.Drift.RunTTL, d.cfg.Drift.ReconcileInterval,
	)
	reconciler.Start()
	// Same backstop for version-lab health runs, which carry the identical
	// stuck-dispatched failure mode in a separate table and reuse the drift
	// TTL/interval (same anchor and reasoning: created_at, one end-of-job
	// callback, no heartbeat).
	healthReconciler := healthreconcile.New(
		repositories.NewHealthRepository(d.database),
		healthFailureNotifier{health: d.health},
		d.cfg.Drift.RunTTL, d.cfg.Drift.ReconcileInterval,
	)
	healthReconciler.Start()
	// API-key expiry notifier: periodic per-user warning emails for keys nearing
	// expiry. Leader-gated like the jobs above so a multi-replica deployment
	// cannot double-send warning emails.
	expiryNotifier := identitynotify.NewAPIKeyExpiryNotifier(
		idstore.NewAPIKeyRepository(d.identityDB),
		idstore.NewUserRepository(d.identityDB),
		func() identitynotify.ExpiryConfig {
			return identitynotify.ExpiryConfig{
				Enabled:        d.cfg.Notifications.Enabled,
				APIKeyExpiring: d.cfg.Notifications.Events.APIKeyExpiring,
				SMTP: identitymailer.Config{
					Host:     d.smtpCfg.Host,
					Port:     d.smtpCfg.Port,
					From:     d.smtpCfg.From,
					Username: d.smtpCfg.Username,
					Password: d.smtpCfg.Password,
					UseTLS:   d.smtpCfg.UseTLS,
				},
				WarningDays:        d.cfg.Notifications.APIKeyExpiryWarningDays,
				CheckIntervalHours: d.cfg.Notifications.APIKeyExpiryCheckIntervalHours,
			}
		},
		identitynotify.ExpiryOptions{ProductName: "Terraform State Manager"},
	)
	go func() { _ = expiryNotifier.Start(context.Background()) }()

	return func() {
		runner.Stop()
		syncer.Stop()
		reconciler.Stop()
		healthReconciler.Stop()
		_ = expiryNotifier.Stop()
	}
}
