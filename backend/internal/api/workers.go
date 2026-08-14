package api

import (
	"context"
	"database/sql"
	"log/slog"

	"github.com/sethbacon/terraform-suite-identity/identity/auditoutbox"
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
	// auditRelay drains the platform-admin audit outbox into identity.audit_logs.
	// Nil on a deployment with no carrier (the nil-DB unit-test rig).
	auditRelay *auditoutbox.Relay
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
	stopRelay := d.startAuditRelay()
	syncer := statesync.New(
		repositories.NewSourceRepository(d.database),
		repositories.NewStateAnalysisRepository(d.database),
		ConnectSource,
	)
	// Bound state_backups (#257). Attached before the leader gate below because
	// the sweep itself rides the periodic cycle, which only the leader runs.
	if d.cfg.BackupRetention.Enabled {
		syncer.EnableBackupRetention(
			repositories.NewStateEditRepository(d.database),
			d.cfg.BackupRetention.Keep,
			d.cfg.BackupRetention.MaxAge,
		)
	}
	d.sources.AttachSyncer(syncer)
	if !d.cfg.Workers.Enabled {
		slog.Info("background workers disabled on this replica (workers.enabled=false); " +
			"schedule firing and periodic state sync run on a worker-enabled replica")
		return stopRelay
	}
	elector := leaderelect.New(d.database, func() (stopWorkers func()) {
		return d.startWorkers(syncer)
	})
	elector.Start()
	return func() {
		elector.Stop()
		stopRelay()
	}
}

// startAuditRelay drains the platform-admin audit outbox into the identity audit
// log, and returns a stop func.
//
// NOT LEADER-GATED, unlike every other periodic loop here, and deliberately so.
// The relay claims with FOR UPDATE SKIP LOCKED, so several replicas take
// disjoint batches instead of colliding — the property the leader election
// exists to provide for the schedule runner and the expiry notifier is one this
// job already has. Gating it would tie the audit trail's liveness to leadership:
// a deployment that runs its workers on one replica (the documented TSM shape)
// would stop delivering audit records the moment that replica went down, and the
// intents would sit undelivered while every other replica reported healthy.
//
// A relay that refuses to start is an ERROR, not a warning: privileged mutations
// would keep writing intents with nothing to drain them.
func (d backgroundWorkerDeps) startAuditRelay() (stop func()) {
	if d.auditRelay == nil {
		return func() {}
	}
	go func() {
		if err := d.auditRelay.Start(context.Background()); err != nil {
			slog.Error("audit outbox relay failed to start; platform-admin audit records "+
				"will accumulate undelivered", "error", err)
		}
	}()
	return func() { _ = d.auditRelay.Stop() }
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
				// notify.SMTPConfig is an ALIAS for identitymailer.Config, so
				// TLSMode copies straight across: both sides are the module's own
				// type and the polarity cannot be inverted in transit.
				SMTP: identitymailer.Config{
					Host:     d.smtpCfg.Host,
					Port:     d.smtpCfg.Port,
					From:     d.smtpCfg.From,
					Username: d.smtpCfg.Username,
					Password: d.smtpCfg.Password,
					TLSMode:  d.smtpCfg.TLSMode,
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
