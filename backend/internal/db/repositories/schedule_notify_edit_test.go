package repositories

import (
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
)

// ---------------------------------------------------------------------------
// ScheduleRepository
// ---------------------------------------------------------------------------

var scheduleCols = []string{"id", "name", "cron_expr", "target_type", "target_config", "enabled",
	"last_run_at", "next_run_at", "last_run_id", "last_status", "created_at", "updated_at"}

func scheduleRow() *sqlmock.Rows {
	return sqlmock.NewRows(scheduleCols).
		AddRow("sc1", "nightly drift", "0 2 * * *", "drift_run", []byte(`{"pipeline_connection_id":"p1"}`), true,
			nil, "2026-06-11 02:00:00", nil, nil, "2026-06-10", "2026-06-10")
}

func TestScheduleRepository_CRUD(t *testing.T) {
	db, mock := newMock(t)
	r := NewScheduleRepository(db)
	next := time.Now().Add(time.Hour)

	mock.ExpectQuery("INSERT INTO schedules").
		WithArgs("nightly drift", "0 2 * * *", "drift_run", `{"pipeline_connection_id":"p1"}`, true, next).
		WillReturnRows(scheduleRow())
	created, err := r.Create(ctx, &Schedule{
		Name: "nightly drift", CronExpr: "0 2 * * *", TargetType: "drift_run",
		TargetConfig: json.RawMessage(`{"pipeline_connection_id":"p1"}`), Enabled: true,
	}, &next)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.NextRunAt == nil || created.LastRunAt != nil {
		t.Errorf("nullable timestamps not mapped: %+v", created)
	}

	// Empty target config defaults to {}.
	mock.ExpectQuery("INSERT INTO schedules").
		WithArgs("s", "@daily", "drift_run", `{}`, false, nil).
		WillReturnRows(scheduleRow())
	if _, err := r.Create(ctx, &Schedule{Name: "s", CronExpr: "@daily", TargetType: "drift_run"}, nil); err != nil {
		t.Fatalf("Create empty config: %v", err)
	}

	mock.ExpectQuery("SELECT .+ FROM schedules ORDER BY created_at DESC").WillReturnRows(scheduleRow())
	list, err := r.List(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("List: %v %d", err, len(list))
	}

	mock.ExpectQuery("SELECT .+ FROM schedules WHERE id").WithArgs("nope").WillReturnError(sql.ErrNoRows)
	if s, err := r.GetByID(ctx, "nope"); err != nil || s != nil {
		t.Errorf("missing schedule should be (nil, nil), got %+v %v", s, err)
	}

	mock.ExpectQuery("UPDATE schedules").WillReturnRows(scheduleRow())
	updated, err := r.Update(ctx, "sc1", &Schedule{Name: "nightly drift", CronExpr: "0 2 * * *", TargetType: "drift_run", Enabled: true}, &next)
	if err != nil || updated == nil {
		t.Fatalf("Update: %v", err)
	}

	// Updating a deleted schedule reports (nil, nil).
	mock.ExpectQuery("UPDATE schedules").WillReturnError(sql.ErrNoRows)
	if s, err := r.Update(ctx, "gone", &Schedule{}, nil); err != nil || s != nil {
		t.Errorf("update of missing schedule should be (nil, nil), got %+v %v", s, err)
	}

	mock.ExpectExec("DELETE FROM schedules").WithArgs("sc1").WillReturnResult(sqlmock.NewResult(0, 1))
	if err := r.Delete(ctx, "sc1"); err != nil {
		t.Errorf("Delete: %v", err)
	}
}

func TestScheduleRepository_DueAndRecord(t *testing.T) {
	db, mock := newMock(t)
	r := NewScheduleRepository(db)
	now := time.Now()

	mock.ExpectQuery("FROM schedules WHERE enabled AND next_run_at IS NOT NULL").WithArgs(now).
		WillReturnRows(scheduleRow())
	due, err := r.GetDue(ctx, now)
	if err != nil || len(due) != 1 {
		t.Fatalf("GetDue: %v %d", err, len(due))
	}

	runID := "d1"
	next := now.Add(24 * time.Hour)
	mock.ExpectExec("UPDATE schedules").
		WithArgs("sc1", now, "dispatched", "d1", next).
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := r.RecordRun(ctx, "sc1", "dispatched", &runID, now, &next); err != nil {
		t.Errorf("RecordRun: %v", err)
	}

	// nil run id / next-run are recorded as NULLs.
	mock.ExpectExec("UPDATE schedules").
		WithArgs("sc1", now, "failed", nil, nil).
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := r.RecordRun(ctx, "sc1", "failed", nil, now, nil); err != nil {
		t.Errorf("RecordRun nil: %v", err)
	}
}

// ---------------------------------------------------------------------------
// NotificationChannelRepository
// ---------------------------------------------------------------------------

var channelCols = []string{"id", "name", "type", "encrypted_target", "events", "enabled",
	"last_status", "last_error", "last_sent_at", "created_at", "updated_at"}

func channelRow() *sqlmock.Rows {
	return sqlmock.NewRows(channelCols).
		AddRow("n1", "ops-slack", "slack", []byte("sealed"), []byte(`{drift_detected,run_failed}`), true,
			nil, nil, nil, "2026-06-10", "2026-06-10")
}

func TestNotificationChannelRepository_CRUD(t *testing.T) {
	db, mock := newMock(t)
	r := NewNotificationChannelRepository(db)

	mock.ExpectQuery("INSERT INTO notification_channels").WillReturnRows(channelRow())
	created, err := r.Create(ctx, &NotificationChannel{
		Name: "ops-slack", Type: "slack", EncryptedTarget: []byte("sealed"),
		Events: []string{"drift_detected", "run_failed"}, Enabled: true,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !created.HasTarget {
		t.Error("HasTarget should be derived from the sealed target")
	}
	if len(created.Events) != 2 {
		t.Errorf("events array not scanned: %+v", created.Events)
	}

	mock.ExpectQuery("SELECT .+ FROM notification_channels ORDER BY").WillReturnRows(channelRow())
	list, err := r.List(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("List: %v %d", err, len(list))
	}

	mock.ExpectQuery("SELECT .+ FROM notification_channels WHERE id").WithArgs("nope").WillReturnError(sql.ErrNoRows)
	if ch, err := r.GetByID(ctx, "nope"); err != nil || ch != nil {
		t.Errorf("missing channel should be (nil, nil), got %+v %v", ch, err)
	}

	mock.ExpectQuery("UPDATE notification_channels").WillReturnRows(channelRow())
	updated, err := r.Update(ctx, "n1", "ops-slack", "slack", []string{"drift_detected"}, true, nil)
	if err != nil || updated == nil {
		t.Fatalf("Update: %v", err)
	}

	mock.ExpectExec("DELETE FROM notification_channels").WithArgs("n1").WillReturnResult(sqlmock.NewResult(0, 1))
	if err := r.Delete(ctx, "n1"); err != nil {
		t.Errorf("Delete: %v", err)
	}
}

func TestNotificationChannelRepository_EventDelivery(t *testing.T) {
	db, mock := newMock(t)
	r := NewNotificationChannelRepository(db)

	mock.ExpectQuery("FROM notification_channels WHERE enabled").WithArgs("drift_detected").
		WillReturnRows(channelRow())
	chans, err := r.ListEnabledForEvent(ctx, "drift_detected")
	if err != nil || len(chans) != 1 {
		t.Fatalf("ListEnabledForEvent: %v %d", err, len(chans))
	}

	now := time.Now()
	mock.ExpectExec("UPDATE notification_channels").
		WithArgs("n1", "sent", "", now).
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := r.RecordDelivery(ctx, "n1", "sent", "", now); err != nil {
		t.Errorf("RecordDelivery: %v", err)
	}
}

// ---------------------------------------------------------------------------
// StateEditRepository
// ---------------------------------------------------------------------------

func TestStateEditRepository(t *testing.T) {
	db, mock := newMock(t)
	r := NewStateEditRepository(db)

	serial := int64(7)
	mock.ExpectQuery("INSERT INTO state_backups").
		WithArgs("s1", "k", []byte(`{"version":4}`), &serial, "alice").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("b1"))
	id, err := r.CreateBackup(ctx, "s1", "k", []byte(`{"version":4}`), &serial, "alice")
	if err != nil || id != "b1" {
		t.Fatalf("CreateBackup: %v %q", err, id)
	}

	mock.ExpectQuery("FROM state_backups").WithArgs("s1", "k").
		WillReturnRows(sqlmock.NewRows([]string{"id", "source_id", "state_key", "serial", "created_by", "created_at"}).
			AddRow("b1", "s1", "k", 7, "alice", "2026-06-10"))
	backups, err := r.ListBackups(ctx, "s1", "k")
	if err != nil || len(backups) != 1 || backups[0].Serial == nil || *backups[0].Serial != 7 {
		t.Fatalf("ListBackups: %v %+v", err, backups)
	}

	mock.ExpectQuery("FROM state_backups WHERE id").WithArgs("b1").
		WillReturnRows(sqlmock.NewRows([]string{"id", "source_id", "state_key", "data", "serial", "created_by", "created_at"}).
			AddRow("b1", "s1", "k", []byte(`{"version":4}`), nil, "alice", "2026-06-10"))
	b, err := r.GetBackup(ctx, "b1")
	if err != nil || b == nil || string(b.Data) != `{"version":4}` {
		t.Fatalf("GetBackup: %v %+v", err, b)
	}
	if b.Serial != nil {
		t.Error("NULL serial should map to nil")
	}

	backupID := "b1"
	before, after := int64(7), int64(8)
	mock.ExpectExec("INSERT INTO state_edits").
		WithArgs("s1", "k", "edit", "alice", &backupID, &before, &after, "success", nil).
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := r.RecordEdit(ctx, &Edit{
		SourceID: "s1", StateKey: "k", Operation: "edit", Actor: "alice",
		BackupID: &backupID, BeforeSerial: &before, AfterSerial: &after, Result: "success",
	}); err != nil {
		t.Errorf("RecordEdit: %v", err)
	}
}
