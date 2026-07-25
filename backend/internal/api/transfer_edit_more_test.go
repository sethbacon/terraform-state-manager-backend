package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"

	"github.com/terraform-state-manager/terraform-state-manager/internal/statesource"
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
	// Target dir exists (so the connector constructs), but the connector's
	// atomic-write temp file ("<key>.tmp") is pre-occupied by a directory, so
	// the write itself fails. A read-only target dir would work on Linux but
	// not Windows, where os.Chmod doesn't enforce Unix write-permission bits
	// on directories; this collision fails open() identically on both.
	blocked := t.TempDir()
	if err := os.Mkdir(filepath.Join(blocked, "copy.tfstate.tmp"), 0o750); err != nil {
		t.Fatal(err)
	}
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

// TestMigrate_SourceLockHeld is a regression test for the CWE-362 gap this fix
// closes: doTransfer previously took no lock at all, so a transfer racing a
// concurrent editor could silently read a source mid-write or have its own
// read clobbered. Now a lock already held on the source key (e.g. by a
// concurrent EditState) must reject the transfer before it touches anything.
func TestMigrate_SourceLockHeld(t *testing.T) {
	e := newSourcesEnv(t)
	dirB := t.TempDir()
	e.seed(t, "app.tfstate", minState(7, "lin-1", "aws_instance.web"))
	if err := os.WriteFile(filepath.Join(e.dir, "app.tfstate.tsmlock"), []byte("held"), 0o600); err != nil {
		t.Fatal(err)
	}

	e.expectSource("s1", e.dir)
	e.expectSource("s2", dirB)

	w := e.do(http.MethodPost, "/api/v1/sources/s1/state/migrate?key=app.tfstate",
		`{"target_source_id":"s2","target_key":"copy.tfstate","decommission":true}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("locked source: status = %d, want 409 (%s)", w.Code, w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(dirB, "copy.tfstate")); !os.IsNotExist(err) {
		t.Error("target must not be written while the source is locked")
	}
	if !strings.Contains(e.read(t, "app.tfstate"), "aws_instance") {
		t.Error("source must not be modified while locked")
	}
}

// TestBackup_TargetLockHeld is the sibling regression test for the second half
// of the same defect: connB.Write ran completely unguarded, so a lock held on
// the TARGET key (by a concurrent editor of the target) did not stop a backup
// from overwriting it.
func TestBackup_TargetLockHeld(t *testing.T) {
	e := newSourcesEnv(t)
	dirB := t.TempDir()
	e.seed(t, "app.tfstate", minState(7, "lin-1", "aws_instance.web"))
	if err := os.WriteFile(filepath.Join(dirB, "copy.tfstate"), []byte(minState(3, "lin-2", "aws_instance.other")), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dirB, "copy.tfstate.tsmlock"), []byte("held"), 0o600); err != nil {
		t.Fatal(err)
	}

	e.expectSource("s1", e.dir)
	e.expectSource("s2", dirB)

	w := e.do(http.MethodPost, "/api/v1/sources/s1/state/backup?key=app.tfstate",
		`{"target_source_id":"s2","target_key":"copy.tfstate"}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("locked target: status = %d, want 409 (%s)", w.Code, w.Body.String())
	}
	if b, err := os.ReadFile(filepath.Join(dirB, "copy.tfstate")); err != nil || !strings.Contains(string(b), `"serial":3`) {
		t.Errorf("target must not be overwritten while locked: %v", err)
	}
	// The source lock (acquired first, since "s1" < "s2") must be released when
	// the second lock acquisition fails — native local locks have no TTL, so a
	// leak here would deadlock every future edit/transfer on this key forever.
	if _, err := os.Stat(filepath.Join(e.dir, "app.tfstate.tsmlock")); !os.IsNotExist(err) {
		t.Error("source lock must be released after the target lock acquisition failed")
	}
}

// TestAcquireTransferLocks_SerializesConcurrentAccess proves, with two real
// goroutines racing over the actual native lock file (no sleeps, channel
// rendezvous only), that a transfer's locks genuinely serialize a concurrent
// writer targeting the same source key for the whole transfer duration — i.e.
// no silent lost update is possible — and that the locks are fully released
// afterward with no leak/deadlock. Run with -race.
func TestAcquireTransferLocks_SerializesConcurrentAccess(t *testing.T) {
	h := &SourcesHandlers{}
	dirA, dirB := t.TempDir(), t.TempDir()
	connA, err := statesource.New("local", map[string]any{"base_path": dirA}, nil)
	if err != nil {
		t.Fatal(err)
	}
	connB, err := statesource.New("local", map[string]any{"base_path": dirB}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dirA, "app.tfstate"), []byte(minState(1, "lin-1")), 0o600); err != nil {
		t.Fatal(err)
	}

	newCtx := func() *gin.Context {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
		return c
	}

	acquired := make(chan bool, 1)
	proceed := make(chan struct{})
	released := make(chan struct{})

	// Holder goroutine: simulates an in-flight transfer that has locked both the
	// source and target keys and is mid-copy.
	go func() {
		release, ok := h.acquireTransferLocks(newCtx(), "s1", connA, "app.tfstate", "s2", connB, "copy.tfstate")
		acquired <- ok
		if !ok {
			return
		}
		<-proceed
		release()
		close(released)
	}()

	if !<-acquired {
		t.Fatal("holder goroutine failed to acquire the transfer locks")
	}

	// While the transfer is still holding its locks, a concurrent write to the
	// SAME source key must be rejected: this is exactly the CWE-362 gap the fix
	// closes — transfer.go previously took no lock at all, so this call would
	// have raced the in-flight transfer's read/write with no serialization.
	if _, ok := h.acquireLock(newCtx(), "s1", connA, "app.tfstate"); ok {
		t.Error("concurrent write to the source key must be rejected while the transfer holds its lock")
	}

	close(proceed)
	select {
	case <-released:
	case <-time.After(5 * time.Second):
		t.Fatal("holder goroutine never released its locks")
	}

	// Once released, the same key must be lockable again — proves the fix
	// doesn't leak or deadlock.
	release, ok := h.acquireLock(newCtx(), "s1", connA, "app.tfstate")
	if !ok {
		t.Fatal("lock must be available again once the transfer releases it")
	}
	release()
}

// errDBForTest is a local sentinel for driving repo errors in this file.
func errDBForTest() error { return os.ErrPermission }

// httpSourceCols is the state_sources row shape for a manually-registered http
// source (mirrors TestEditState_AbortsWhenReadFails's precedent) — needed
// because expectSource always hardcodes type "local".
var httpSourceCols = []string{"id", "name", "type", "endpoint", "config", "scope", "encrypted_credentials", "created_at", "updated_at"}

// TestMigrate_DecommissionSkippedOnSourceDrift covers the pre-decommission
// conflict check itself: reviewer feedback found it had never been driven to
// actually detect a mismatch. Source A is an http backend (not local) because
// the real on-disk local connector has no way to return different content
// between the transfer's initial read and the pre-decommission re-read within
// one synchronous request — an http backend lets the test server do that.
func TestMigrate_DecommissionSkippedOnSourceDrift(t *testing.T) {
	e := newSourcesEnv(t)
	dirB := t.TempDir()

	reads := 0
	wrote := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			reads++
			if reads == 1 {
				_, _ = w.Write([]byte(minState(7, "lin-1", "aws_instance.web")))
			} else {
				// Someone else edited the source between the transfer's read and the
				// pre-decommission re-check.
				_, _ = w.Write([]byte(minState(9, "lin-1", "aws_instance.web")))
			}
		default:
			wrote = true
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()

	cfg, _ := json.Marshal(map[string]any{"address": srv.URL})
	e.mock.ExpectQuery("SELECT .+ FROM state_sources WHERE id").WithArgs("s1").
		WillReturnRows(sqlmock.NewRows(httpSourceCols).
			AddRow("s1", "drifting-http", "http", "", cfg, []byte(`{}`), nil, "2026-06-11", "2026-06-11"))
	e.expectSource("s2", dirB)
	// s1 has no lock_address, so acquireLock falls back to the app-level DB lock:
	// reap, then acquire (see TestEditState_AbortsWhenReadFails for precedent).
	e.mock.ExpectExec("DELETE FROM state_locks").WillReturnResult(sqlmock.NewResult(0, 0))
	e.mock.ExpectQuery("INSERT INTO state_locks").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("lk1"))
	e.mock.ExpectQuery("INSERT INTO state_transfers").
		WillReturnRows(transferReturn("migrate", "success", true, false))
	e.mock.ExpectExec("DELETE FROM state_locks").WillReturnResult(sqlmock.NewResult(0, 1))

	w := e.do(http.MethodPost, "/api/v1/sources/s1/state/migrate?key=app.tfstate",
		`{"target_source_id":"s2","target_key":"copy.tfstate","decommission":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("migrate: status = %d (%s)", w.Code, w.Body.String())
	}
	if reads != 2 {
		t.Fatalf("expected the initial read plus one pre-decommission re-check, got %d reads", reads)
	}
	if wrote {
		t.Error("decommission must not write back to a source that changed since the transfer read")
	}
	if err := e.mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestMigrate_ForceOverridesDecommissionConflict is the sibling of
// TestMigrate_DecommissionSkippedOnSourceDrift: force=true must skip the
// pre-decommission re-check entirely (not just tolerate a mismatch it found),
// so the source is emptied even though it changed since the transfer read.
func TestMigrate_ForceOverridesDecommissionConflict(t *testing.T) {
	e := newSourcesEnv(t)
	dirB := t.TempDir()

	reads := 0
	wrote := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			reads++
			if reads == 1 {
				_, _ = w.Write([]byte(minState(7, "lin-1", "aws_instance.web")))
			} else {
				_, _ = w.Write([]byte(minState(9, "lin-1", "aws_instance.web")))
			}
		default:
			wrote = true
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()

	cfg, _ := json.Marshal(map[string]any{"address": srv.URL})
	e.mock.ExpectQuery("SELECT .+ FROM state_sources WHERE id").WithArgs("s1").
		WillReturnRows(sqlmock.NewRows(httpSourceCols).
			AddRow("s1", "drifting-http", "http", "", cfg, []byte(`{}`), nil, "2026-06-11", "2026-06-11"))
	e.expectSource("s2", dirB)
	e.mock.ExpectExec("DELETE FROM state_locks").WillReturnResult(sqlmock.NewResult(0, 0))
	e.mock.ExpectQuery("INSERT INTO state_locks").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("lk1"))
	e.mock.ExpectQuery("INSERT INTO state_backups").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("b1"))
	e.mock.ExpectExec("INSERT INTO state_edits").WillReturnResult(sqlmock.NewResult(0, 1))
	e.mock.ExpectQuery("INSERT INTO state_transfers").
		WillReturnRows(transferReturn("migrate", "success", true, true))
	e.mock.ExpectExec("DELETE FROM state_locks").WillReturnResult(sqlmock.NewResult(0, 1))

	w := e.do(http.MethodPost, "/api/v1/sources/s1/state/migrate?key=app.tfstate&force=true",
		`{"target_source_id":"s2","target_key":"copy.tfstate","decommission":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("forced migrate: status = %d (%s)", w.Code, w.Body.String())
	}
	if reads != 1 {
		t.Errorf("force=true must skip the pre-decommission re-read entirely, got %d source reads", reads)
	}
	if !wrote {
		t.Error("force=true must still decommission (write the emptied state back)")
	}
	if !strings.Contains(w.Body.String(), `"decommissioned":true`) {
		t.Errorf("response must report the source as decommissioned: %s", w.Body.String())
	}
	if err := e.mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}
