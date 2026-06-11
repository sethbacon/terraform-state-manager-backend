package api

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
)

var transferHTTPCols = []string{"id", "mode", "source_id", "source_key", "target_source_id", "target_key", "status", "verified", "decommissioned", "detail", "actor", "created_at"}

func transferReturn(mode, status string, verified any, decommissioned bool) *sqlmock.Rows {
	return sqlmock.NewRows(transferHTTPCols).
		AddRow("t1", mode, "s1", "app.tfstate", "s2", "copy.tfstate", status, verified, decommissioned, "", "", "2026-06-10")
}

func TestMigrate_VerifiedWithDecommission(t *testing.T) {
	e := newSourcesEnv(t)
	dirB := t.TempDir()
	e.seed(t, "app.tfstate", minState(7, "lin-1", "aws_instance.web"))

	e.expectSource("s1", e.dir)
	e.expectSource("s2", dirB)
	// Decommission: pre-decommission backup INSERT, then state_edits record.
	e.mock.ExpectQuery("INSERT INTO state_backups").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("b1"))
	e.mock.ExpectExec("INSERT INTO state_edits").WillReturnResult(sqlmock.NewResult(0, 1))
	e.mock.ExpectQuery("INSERT INTO state_transfers").
		WillReturnRows(transferReturn("migrate", "success", true, true))

	w := e.do(http.MethodPost, "/api/v1/sources/s1/state/migrate?key=app.tfstate",
		`{"target_source_id":"s2","target_key":"copy.tfstate","decommission":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("migrate: status = %d (%s)", w.Code, w.Body.String())
	}

	// Target received the state verbatim.
	if b, err := os.ReadFile(filepath.Join(dirB, "copy.tfstate")); err != nil || !strings.Contains(string(b), `"serial":7`) {
		t.Errorf("target state wrong: %v", err)
	}
	// Source was emptied (serial bumped, no resources) — only after a verified
	// copy and a successful pre-decommission backup.
	src := e.read(t, "app.tfstate")
	if !strings.Contains(src, `"serial":8`) || strings.Contains(src, "aws_instance") {
		t.Errorf("source not decommissioned correctly: %s", src)
	}
	if err := e.mock.ExpectationsWereMet(); err != nil {
		t.Errorf("decommission must back up first and record the edit: %v", err)
	}
}

func TestMigrate_DecommissionSkippedWhenBackupFails(t *testing.T) {
	e := newSourcesEnv(t)
	dirB := t.TempDir()
	e.seed(t, "app.tfstate", minState(7, "lin-1", "aws_instance.web"))

	e.expectSource("s1", e.dir)
	e.expectSource("s2", dirB)
	// The pre-decommission backup fails (no expectation rows → error).
	e.mock.ExpectQuery("INSERT INTO state_backups").WillReturnError(errDBForTest())
	e.mock.ExpectQuery("INSERT INTO state_transfers").
		WillReturnRows(transferReturn("migrate", "success", true, false))

	w := e.do(http.MethodPost, "/api/v1/sources/s1/state/migrate?key=app.tfstate",
		`{"target_source_id":"s2","target_key":"copy.tfstate","decommission":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("migrate: status = %d (%s)", w.Code, w.Body.String())
	}
	// Source must be PRESERVED: no recoverable backup means no decommission.
	if !strings.Contains(e.read(t, "app.tfstate"), "aws_instance") {
		t.Error("source was emptied despite a failed pre-decommission backup")
	}
}

func TestMigrate_WriteToTargetFails(t *testing.T) {
	e := newSourcesEnv(t)
	e.seed(t, "app.tfstate", minState(7, "lin-1", "aws_instance.web"))

	e.expectSource("s1", e.dir)
	// Target dir exists (so the connector constructs) but is read-only, so
	// the write itself fails.
	blocked := filepath.Join(t.TempDir(), "blocked")
	if err := os.Mkdir(blocked, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(blocked, 0o700) })
	e.expectSource("s2", blocked)
	e.mock.ExpectQuery("INSERT INTO state_transfers").
		WillReturnRows(transferReturn("backup", "failed", nil, false))

	w := e.do(http.MethodPost, "/api/v1/sources/s1/state/backup?key=app.tfstate",
		`{"target_source_id":"s2","target_key":"copy.tfstate"}`)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("failed write: status = %d, want 502 (%s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "failed") {
		t.Errorf("failure detail missing: %s", w.Body.String())
	}
}

func TestEditState_LockConflict(t *testing.T) {
	e := newSourcesEnv(t)
	e.seed(t, "app.tfstate", minState(7, "lin-1", "aws_instance.web"))
	// Simulate another writer holding the local connector's native lock.
	if err := os.WriteFile(filepath.Join(e.dir, "app.tfstate.tsmlock"), []byte("held"), 0o600); err != nil {
		t.Fatal(err)
	}

	e.expectSource("s1", e.dir)
	w := e.do(http.MethodPut, "/api/v1/sources/s1/state/raw?key=app.tfstate",
		minState(8, "lin-1", "aws_instance.web"))
	if w.Code != http.StatusConflict {
		t.Fatalf("locked state: status = %d, want 409 (%s)", w.Code, w.Body.String())
	}
	if !strings.Contains(e.read(t, "app.tfstate"), `"serial":7`) {
		t.Error("locked edit must not write")
	}
}

// errDBForTest is a local sentinel for driving repo errors in this file.
func errDBForTest() error { return os.ErrPermission }
