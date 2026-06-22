package repositories

import (
	"database/sql"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
)

// ---------------------------------------------------------------------------
// DriftRepository
// ---------------------------------------------------------------------------

var driftCols = []string{"id", "pipeline_connection_id", "source_id", "state_key", "repo_ref", "working_dir",
	"status", "added", "changed", "destroyed", "drifted", "summary", "detail", "callback_token", "actor",
	"created_at", "updated_at"}

func driftRow(token string) *sqlmock.Rows {
	return sqlmock.NewRows(driftCols).
		AddRow("d1", "p1", "s1", "app.tfstate", "refs/heads/main", "infra/",
			"completed", 1, 2, 0, true, []byte(`{"resources":[]}`), "", token, "alice",
			"2026-06-10", "2026-06-10")
}

func TestDriftRepository_CreateAndGet(t *testing.T) {
	db, mock := newMock(t)
	r := NewDriftRepository(db)

	mock.ExpectQuery("INSERT INTO drift_runs").WillReturnRows(driftRow("tok-1"))
	conn := "p1"
	created, err := r.Create(ctx, &DriftRun{PipelineConnectionID: &conn, StateKey: "app.tfstate", Status: "pending", CallbackToken: "tok-1"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.Drifted == nil || !*created.Drifted || *created.Added != 1 || *created.Changed != 2 {
		t.Errorf("nullable result fields not mapped: %+v", created)
	}

	mock.ExpectQuery("SELECT .+ FROM drift_runs WHERE id").WithArgs("nope").WillReturnError(sql.ErrNoRows)
	if d, err := r.GetByID(ctx, "nope"); err != nil || d != nil {
		t.Errorf("missing run should be (nil, nil), got %+v %v", d, err)
	}
}

func TestDriftRepository_ListHidesCallbackToken(t *testing.T) {
	db, mock := newMock(t)
	r := NewDriftRepository(db)

	mock.ExpectQuery("SELECT .+ FROM drift_runs ORDER BY created_at DESC LIMIT").WithArgs(50).
		WillReturnRows(driftRow("secret-token"))
	out, err := r.List(ctx, 0) // 0 → default limit 50
	if err != nil || len(out) != 1 {
		t.Fatalf("List: %v %d", err, len(out))
	}
	if out[0].CallbackToken != "" {
		t.Error("List leaked the callback token")
	}

	// Out-of-range limits clamp to the default.
	mock.ExpectQuery("SELECT .+ FROM drift_runs ORDER BY created_at DESC LIMIT").WithArgs(50).
		WillReturnRows(driftRow(""))
	if _, err := r.List(ctx, 9999); err != nil {
		t.Fatalf("List clamp: %v", err)
	}
}

func TestDriftRepository_ConsumeCallbackToken(t *testing.T) {
	db, mock := newMock(t)
	r := NewDriftRepository(db)

	// First consume matches a row → token spent.
	mock.ExpectExec("UPDATE drift_runs SET callback_token=''").WithArgs("d1", "tok-1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	ok, err := r.ConsumeCallbackToken(ctx, "d1", "tok-1")
	if err != nil || !ok {
		t.Fatalf("first consume should succeed: %v %v", ok, err)
	}

	// Replay: no row matches the already-cleared token.
	mock.ExpectExec("UPDATE drift_runs SET callback_token=''").WithArgs("d1", "tok-1").
		WillReturnResult(sqlmock.NewResult(0, 0))
	ok, err = r.ConsumeCallbackToken(ctx, "d1", "tok-1")
	if err != nil || ok {
		t.Errorf("replayed consume must be rejected: %v %v", ok, err)
	}

	// Empty tokens never consume (no query at all).
	ok, err = r.ConsumeCallbackToken(ctx, "d1", "")
	if err != nil || ok {
		t.Errorf("empty token must be rejected without a query: %v %v", ok, err)
	}
}

func TestDriftRepository_Updates(t *testing.T) {
	db, mock := newMock(t)
	r := NewDriftRepository(db)

	mock.ExpectExec("UPDATE drift_runs SET status").WithArgs("d1", "running", "").
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := r.UpdateStatus(ctx, "d1", "running", ""); err != nil {
		t.Errorf("UpdateStatus: %v", err)
	}

	mock.ExpectExec("UPDATE drift_runs").
		WithArgs("d1", "completed", 1, 2, 0, true, `{"x":1}`, "done").
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := r.UpdateResult(ctx, "d1", "completed", 1, 2, 0, true, []byte(`{"x":1}`), "done"); err != nil {
		t.Errorf("UpdateResult: %v", err)
	}
}

// ---------------------------------------------------------------------------
// HealthRepository
// ---------------------------------------------------------------------------

var healthCols = []string{"id", "pipeline_connection_id", "repo_ref", "working_dir", "terraform_version",
	"provider_versions", "module_versions", "registry_host", "status", "init_ok", "plan_ok", "success",
	"summary", "detail", "callback_token", "actor", "created_at", "updated_at"}

func healthRow(token string) *sqlmock.Rows {
	return sqlmock.NewRows(healthCols).
		AddRow("h1", "p1", "refs/heads/main", "infra/", "1.9.5",
			[]byte(`{"aws":"5.0.0"}`), []byte(`{}`), "registry.example.com", "completed", true, true, true,
			[]byte(`{}`), "", token, "alice", "2026-06-10", "2026-06-10")
}

func TestHealthRepository_CreateListGet(t *testing.T) {
	db, mock := newMock(t)
	r := NewHealthRepository(db)

	mock.ExpectQuery("INSERT INTO health_runs").WillReturnRows(healthRow("tok-1"))
	conn := "p1"
	created, err := r.Create(ctx, &HealthRun{PipelineConnectionID: &conn, Status: "pending", CallbackToken: "tok-1"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.TerraformVersion != "1.9.5" {
		t.Errorf("fields not mapped: %+v", created)
	}

	mock.ExpectQuery("SELECT .+ FROM health_runs ORDER BY created_at DESC LIMIT").WithArgs(50, 0).
		WillReturnRows(healthRow("secret"))
	out, err := r.List(ctx, -1, 0, "")
	if err != nil || len(out) != 1 {
		t.Fatalf("List: %v %d", err, len(out))
	}
	if out[0].CallbackToken != "" {
		t.Error("List leaked the callback token")
	}

	// Filtered + windowed list reaches the query as (status, limit, offset).
	mock.ExpectQuery("SELECT .+ FROM health_runs WHERE status = .+ ORDER BY .+ LIMIT .+ OFFSET").
		WithArgs("failed", 10, 20).WillReturnRows(healthRow("secret"))
	if _, err := r.List(ctx, 10, 20, "failed"); err != nil {
		t.Fatalf("filtered List: %v", err)
	}

	// CountRuns totals, optionally by status.
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM health_runs WHERE status =`).WithArgs("failed").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(4))
	if n, err := r.CountRuns(ctx, "failed"); err != nil || n != 4 {
		t.Fatalf("CountRuns: %v %d", err, n)
	}

	mock.ExpectQuery("SELECT .+ FROM health_runs WHERE id").WithArgs("nope").WillReturnError(sql.ErrNoRows)
	if h, err := r.GetByID(ctx, "nope"); err != nil || h != nil {
		t.Errorf("missing run should be (nil, nil), got %+v %v", h, err)
	}
}

func TestHealthRepository_TokenAndUpdates(t *testing.T) {
	db, mock := newMock(t)
	r := NewHealthRepository(db)

	mock.ExpectExec("UPDATE health_runs SET callback_token=''").WithArgs("h1", "tok").
		WillReturnResult(sqlmock.NewResult(0, 1))
	ok, err := r.ConsumeCallbackToken(ctx, "h1", "tok")
	if err != nil || !ok {
		t.Fatalf("consume: %v %v", ok, err)
	}
	if ok, _ := r.ConsumeCallbackToken(ctx, "h1", ""); ok {
		t.Error("empty token must be rejected")
	}

	mock.ExpectExec("UPDATE health_runs SET status").WithArgs("h1", "running", "").
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := r.UpdateStatus(ctx, "h1", "running", ""); err != nil {
		t.Errorf("UpdateStatus: %v", err)
	}

	mock.ExpectExec("UPDATE health_runs").
		WithArgs("h1", "completed", true, true, true, `{}`, "").
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := r.UpdateResult(ctx, "h1", "completed", true, true, true, []byte(`{}`), ""); err != nil {
		t.Errorf("UpdateResult: %v", err)
	}
}
