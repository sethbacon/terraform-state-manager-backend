package statesync

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"

	"github.com/terraform-state-manager/terraform-state-manager/internal/analyzer"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
	"github.com/terraform-state-manager/terraform-state-manager/internal/statesource"
)

var ctx = context.Background()

// TestMain permits the OS temp directory as a local state-source root: the sync
// tests drive real local connectors over t.TempDir() directories, and local
// sources are confined to configured roots (internal/statesource/roots.go),
// which are empty — and therefore permit nothing — by default.
func TestMain(m *testing.M) {
	_ = statesource.ConfigureLocalRoots([]string{os.TempDir()})
	os.Exit(m.Run())
}

func newMock(t *testing.T) (*sql.DB, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := newSQLMock()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	// Sync reads run concurrently; expectation order is nondeterministic.
	mock.MatchExpectationsInOrder(false)
	t.Cleanup(func() { db.Close() })
	return db, mock
}

func minState(serial int64, resources ...string) string {
	type res struct {
		Mode      string           `json:"mode"`
		Type      string           `json:"type"`
		Name      string           `json:"name"`
		Provider  string           `json:"provider"`
		Instances []map[string]any `json:"instances"`
	}
	out := struct {
		Version          int    `json:"version"`
		TerraformVersion string `json:"terraform_version"`
		Serial           int64  `json:"serial"`
		Lineage          string `json:"lineage"`
		Resources        []res  `json:"resources"`
	}{Version: 4, TerraformVersion: "1.9.5", Serial: serial, Lineage: "lin-1"}
	for _, r := range resources {
		out.Resources = append(out.Resources, res{
			Mode: "managed", Type: r, Name: "x",
			Provider:  `provider["registry.terraform.io/hashicorp/aws"]`,
			Instances: []map[string]any{{"attributes": map[string]any{}}},
		})
	}
	b, _ := json.Marshal(out)
	return string(b)
}

func seed(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatalf("seed %s: %v", name, err)
	}
}

// bareFileMarker is the pre-version stored marker: the raw size|last-modified
// change token with no analyzer-version suffix, as written by builds from before
// the version fold-in existed. Used to model byte-unchanged states whose counts
// were computed by an older analyzer.
func bareFileMarker(t *testing.T, dir, name string) string {
	t.Helper()
	info, err := os.Stat(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("stat %s: %v", name, err)
	}
	return fmt.Sprintf("%d|%s", info.Size(), info.ModTime().UTC().Format(time.RFC3339Nano))
}

// fileMarker computes the marker the syncer now derives for a local state: the
// byte change token with the current analyzer version folded in, so tests can
// pre-load "unchanged" (current-version) markers.
func fileMarker(t *testing.T, dir, name string) string {
	t.Helper()
	return bareFileMarker(t, dir, name) + "|a" + strconv.Itoa(analyzer.AnalysisVersion)
}

func localSource(dir string) *repositories.Source {
	return &repositories.Source{
		ID: "s1", Name: "demo", Type: "local",
		Config: map[string]any{"base_path": dir},
	}
}

func connectLocal(s *repositories.Source) (statesource.Connector, error) {
	return statesource.New(s.Type, s.Config, nil)
}

func newSyncer(db *sql.DB, connect Connect) *Syncer {
	s := New(
		repositories.NewSourceRepository(db),
		repositories.NewStateAnalysisRepository(db),
		connect,
	)
	s.retryDelay = time.Millisecond // no need to be polite to fakes
	return s
}

// fakeConn scripts List/Read failures for the error paths.
type fakeConn struct {
	refs      []statesource.StateRef
	listErr   error
	readErr   error
	data      map[string]string
	failFirst map[string]int // key -> remaining scripted failures (e.g. 429 bursts)
	mu        sync.Mutex
}

func (f *fakeConn) List(context.Context) ([]statesource.StateRef, error) {
	return f.refs, f.listErr
}
func (f *fakeConn) Read(_ context.Context, key string) (*statesource.RawState, error) {
	f.mu.Lock()
	if n := f.failFirst[key]; n > 0 {
		f.failFirst[key] = n - 1
		f.mu.Unlock()
		return nil, errors.New("state download returned 429")
	}
	f.mu.Unlock()
	if f.readErr != nil {
		return nil, f.readErr
	}
	return &statesource.RawState{Key: key, Data: []byte(f.data[key])}, nil
}
func (f *fakeConn) Write(context.Context, string, []byte) error { return nil }
func (f *fakeConn) Delete(context.Context, string) error        { return nil }

func sourceRows(dir string) *sqlmock.Rows {
	cfg, _ := json.Marshal(map[string]any{"base_path": dir})
	return sqlmock.NewRows([]string{"id", "name", "type", "endpoint", "config", "scope", "encrypted_credentials", "created_at", "updated_at"}).
		AddRow("s1", "demo", "local", "", cfg, []byte(`{}`), nil, "2026-06-10", "2026-06-10")
}

func twoSourceRows(dir1, dir2 string) *sqlmock.Rows {
	cfg1, _ := json.Marshal(map[string]any{"base_path": dir1})
	cfg2, _ := json.Marshal(map[string]any{"base_path": dir2})
	return sqlmock.NewRows([]string{"id", "name", "type", "endpoint", "config", "scope", "encrypted_credentials", "created_at", "updated_at"}).
		AddRow("s1", "demo", "local", "", cfg1, []byte(`{}`), nil, "2026-06-10", "2026-06-10").
		AddRow("s2", "other", "local", "", cfg2, []byte(`{}`), nil, "2026-06-10", "2026-06-10")
}

func TestSyncAll_FirstBackfillReadsEverything(t *testing.T) {
	db, mock := newMock(t)
	dir := t.TempDir()
	seed(t, dir, "app.tfstate", minState(7, "aws_instance", "aws_vpc"))
	seed(t, dir, "net.tfstate", minState(3, "aws_subnet"))

	mock.ExpectQuery("SELECT .+ FROM state_sources ORDER BY").WillReturnRows(sourceRows(dir))
	mock.ExpectQuery("SELECT state_key, version_marker FROM state_analyses").WithArgs("s1").
		WillReturnRows(sqlmock.NewRows([]string{"state_key", "version_marker"}))
	mock.ExpectExec("INSERT INTO state_analyses").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO state_analyses").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO state_analysis_history").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO state_analysis_history").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("DELETE FROM state_analyses WHERE source_id").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("DELETE FROM state_analysis_history").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("INSERT INTO source_sync_status").WithArgs("s1", 2, 0, "").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := newSyncer(db, connectLocal).SyncAll(ctx); err != nil {
		t.Fatalf("SyncAll: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestSyncAll_UnchangedStatesAreNotReRead(t *testing.T) {
	db, mock := newMock(t)
	dir := t.TempDir()
	seed(t, dir, "app.tfstate", minState(7, "aws_instance"))
	seed(t, dir, "stale.tfstate", minState(1, "aws_vpc"))

	// app matches its stored marker; stale's marker moved -> exactly one
	// re-read/upsert. A third stored key vanished from disk -> pruned.
	mock.ExpectQuery("SELECT .+ FROM state_sources ORDER BY").WillReturnRows(sourceRows(dir))
	mock.ExpectQuery("SELECT state_key, version_marker FROM state_analyses").WithArgs("s1").
		WillReturnRows(sqlmock.NewRows([]string{"state_key", "version_marker"}).
			AddRow("app.tfstate", fileMarker(t, dir, "app.tfstate")).
			AddRow("stale.tfstate", "old-marker").
			AddRow("deleted.tfstate", "whatever"))
	mock.ExpectExec("INSERT INTO state_analyses").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO state_analysis_history").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("DELETE FROM state_analyses WHERE source_id").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("DELETE FROM state_analysis_history").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("INSERT INTO source_sync_status").WithArgs("s1", 2, 0, "").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := newSyncer(db, connectLocal).SyncAll(ctx); err != nil {
		t.Fatalf("SyncAll: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestSyncAll_AnalyzerVersionBumpForcesReanalyze(t *testing.T) {
	db, mock := newMock(t)
	dir := t.TempDir()
	seed(t, dir, "legacy.tfstate", minState(1, "aws_instance"))

	// The state's bytes never changed, but its stored marker is the pre-version
	// (bare) format: it was last analyzed before AnalysisVersion existed. The
	// folded-in version must no longer match, forcing exactly one re-read +
	// re-analysis so its stale counts refresh. This is the persisted-Reports
	// staleness fix for long-static 0.11.x states.
	mock.ExpectQuery("SELECT .+ FROM state_sources ORDER BY").WillReturnRows(sourceRows(dir))
	mock.ExpectQuery("SELECT state_key, version_marker FROM state_analyses").WithArgs("s1").
		WillReturnRows(sqlmock.NewRows([]string{"state_key", "version_marker"}).
			AddRow("legacy.tfstate", bareFileMarker(t, dir, "legacy.tfstate")))
	mock.ExpectExec("INSERT INTO state_analyses").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO state_analysis_history").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("DELETE FROM state_analyses WHERE source_id").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("DELETE FROM state_analysis_history").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("INSERT INTO source_sync_status").WithArgs("s1", 1, 0, "").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := newSyncer(db, connectLocal).SyncAll(ctx); err != nil {
		t.Fatalf("SyncAll: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestSyncAll_SourceFailuresAreRecordedNotFatal(t *testing.T) {
	// Connect failure.
	db, mock := newMock(t)
	mock.ExpectQuery("SELECT .+ FROM state_sources ORDER BY").WillReturnRows(sourceRows("/x"))
	mock.ExpectExec("INSERT INTO source_sync_status").WithArgs("s1", 0, 0, "connect: nope").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("DELETE FROM state_analysis_history").WillReturnResult(sqlmock.NewResult(0, 0))
	s := newSyncer(db, func(*repositories.Source) (statesource.Connector, error) {
		return nil, errors.New("nope")
	})
	if err := s.SyncAll(ctx); err != nil {
		t.Fatalf("SyncAll: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}

	// List failure.
	db2, mock2 := newMock(t)
	mock2.ExpectQuery("SELECT .+ FROM state_sources ORDER BY").WillReturnRows(sourceRows("/x"))
	mock2.ExpectExec("INSERT INTO source_sync_status").WithArgs("s1", 0, 0, "list: hcp 429").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock2.ExpectExec("DELETE FROM state_analysis_history").WillReturnResult(sqlmock.NewResult(0, 0))
	s2 := newSyncer(db2, func(*repositories.Source) (statesource.Connector, error) {
		return &fakeConn{listErr: errors.New("hcp 429")}, nil
	})
	if err := s2.SyncAll(ctx); err != nil {
		t.Fatalf("SyncAll: %v", err)
	}
	if err := mock2.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// TestSyncAll_SourcesRunConcurrently: sources reconcile in parallel (bounded by
// sourceConcurrency), so a slow source no longer serializes the fleet. Three
// sources each block in List until every one of them has started — with the old
// serial loop this would deadlock; with the pool (3 slots) all proceed.
func TestSyncAll_SourcesRunConcurrently(t *testing.T) {
	db, mock := newMock(t)
	cfg, _ := json.Marshal(map[string]any{"base_path": "/x"})
	rows := sqlmock.NewRows([]string{"id", "name", "type", "endpoint", "config", "scope", "encrypted_credentials", "created_at", "updated_at"})
	for i := 1; i <= 3; i++ {
		rows.AddRow(fmt.Sprintf("s%d", i), fmt.Sprintf("src-%d", i), "local", "", cfg, []byte(`{}`), nil, "2026-06-10", "2026-06-10")
	}
	mock.ExpectQuery("SELECT .+ FROM state_sources ORDER BY").WillReturnRows(rows)
	for range [3]struct{}{} {
		mock.ExpectExec("INSERT INTO source_sync_status").WillReturnResult(sqlmock.NewResult(0, 1))
	}
	mock.ExpectExec("DELETE FROM state_analysis_history").WillReturnResult(sqlmock.NewResult(0, 0))

	var started sync.WaitGroup
	started.Add(3)
	release := make(chan struct{})
	s := newSyncer(db, func(*repositories.Source) (statesource.Connector, error) {
		started.Done() // signal this source's cycle began
		<-release      // ...and hold it until all three have begun
		return nil, errors.New("done")
	})

	go func() {
		started.Wait() // only reachable when all 3 run concurrently
		close(release)
	}()
	done := make(chan error, 1)
	go func() { done <- s.SyncAll(ctx) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("SyncAll: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("SyncAll deadlocked — sources are not running concurrently")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestSyncAll_ReadAndAnalyzeErrorsCount(t *testing.T) {
	db, mock := newMock(t)
	now := time.Now()
	conn := &fakeConn{
		refs: []statesource.StateRef{
			{Key: "bad-json", LastModified: &now},
			{Key: "unreadable", LastModified: &now},
		},
		data: map[string]string{"bad-json": "not terraform state"},
	}
	// One ref fails analysis, the other fails read (scripted via readErr on a
	// second connector run is not possible per-key, so bad-json covers analyze
	// and an empty data map entry covers "no state" handling).
	conn.data["unreadable"] = `{"version":0}` // analyzer rejects: not a state file

	mock.ExpectQuery("SELECT .+ FROM state_sources ORDER BY").WillReturnRows(sourceRows("/x"))
	mock.ExpectQuery("SELECT state_key, version_marker FROM state_analyses").WithArgs("s1").
		WillReturnRows(sqlmock.NewRows([]string{"state_key", "version_marker"}))
	mock.ExpectExec("DELETE FROM state_analyses WHERE source_id").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("INSERT INTO source_sync_status").WithArgs("s1", 2, 2, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("DELETE FROM state_analysis_history").WillReturnResult(sqlmock.NewResult(0, 0))

	s := newSyncer(db, func(*repositories.Source) (statesource.Connector, error) { return conn, nil })
	if err := s.SyncAll(ctx); err != nil {
		t.Fatalf("SyncAll: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestSyncAll_TransientThrottleClearsOnRetry(t *testing.T) {
	db, mock := newMock(t)
	now := time.Now()
	conn := &fakeConn{
		refs:      []statesource.StateRef{{Key: "throttled", LastModified: &now}},
		data:      map[string]string{"throttled": minState(1, "aws_instance")},
		failFirst: map[string]int{"throttled": 1}, // first read 429s, retry succeeds
	}

	mock.ExpectQuery("SELECT .+ FROM state_sources ORDER BY").WillReturnRows(sourceRows("/x"))
	mock.ExpectQuery("SELECT state_key, version_marker FROM state_analyses").WithArgs("s1").
		WillReturnRows(sqlmock.NewRows([]string{"state_key", "version_marker"}))
	mock.ExpectExec("INSERT INTO state_analyses").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO state_analysis_history").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("DELETE FROM state_analyses WHERE source_id").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("DELETE FROM state_analysis_history").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("INSERT INTO source_sync_status").WithArgs("s1", 1, 0, "").
		WillReturnResult(sqlmock.NewResult(0, 1))

	s := newSyncer(db, func(*repositories.Source) (statesource.Connector, error) { return conn, nil })
	if err := s.SyncAll(ctx); err != nil {
		t.Fatalf("SyncAll: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestSyncAll_Serialization(t *testing.T) {
	db, mock := newMock(t)
	_ = mock
	s := newSyncer(db, connectLocal)
	s.runMu.Lock()
	defer s.runMu.Unlock()
	if err := s.SyncAll(ctx); err != ErrSyncInProgress {
		t.Errorf("SyncAll while locked = %v, want ErrSyncInProgress", err)
	}
}

func TestSyncAll_SourcesListError(t *testing.T) {
	db, mock := newMock(t)
	mock.ExpectQuery("SELECT .+ FROM state_sources ORDER BY").WillReturnError(errors.New("db down"))
	if err := newSyncer(db, connectLocal).SyncAll(ctx); err == nil {
		t.Error("SyncAll: expected error when sources cannot be listed")
	}
}

func TestSyncSources_ScopesToNamedSource(t *testing.T) {
	db, mock := newMock(t)
	dir1, dir2 := t.TempDir(), t.TempDir()
	seed(t, dir1, "app.tfstate", minState(7, "aws_instance"))

	// Two sources are configured but only s1 is requested. Assert at the connect
	// boundary that s2 is never touched, so a filtered Reports refresh reconciles
	// just its own source instead of the whole fleet.
	var mu sync.Mutex
	connected := map[string]bool{}
	connect := func(s *repositories.Source) (statesource.Connector, error) {
		mu.Lock()
		connected[s.ID] = true
		mu.Unlock()
		return connectLocal(s)
	}

	mock.ExpectQuery("SELECT .+ FROM state_sources ORDER BY").WillReturnRows(twoSourceRows(dir1, dir2))
	mock.ExpectQuery("SELECT state_key, version_marker FROM state_analyses").WithArgs("s1").
		WillReturnRows(sqlmock.NewRows([]string{"state_key", "version_marker"}))
	mock.ExpectExec("INSERT INTO state_analyses").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO state_analysis_history").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("DELETE FROM state_analyses WHERE source_id").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("INSERT INTO source_sync_status").WithArgs("s1", 1, 0, "").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := newSyncer(db, connect).SyncSources(ctx, []string{"s1"}); err != nil {
		t.Fatalf("SyncSources: %v", err)
	}
	if !connected["s1"] {
		t.Error("s1 should have been reconciled")
	}
	if connected["s2"] {
		t.Error("s2 was reconciled but was not in the requested id set")
	}
	// SyncSources leaves history pruning to the full cycle: no DELETE history.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestSyncSources_EmptyIsNoop(t *testing.T) {
	db, _ := newMock(t)
	// No expectations: an empty id list must not even list sources.
	if err := newSyncer(db, connectLocal).SyncSources(ctx, nil); err != nil {
		t.Fatalf("SyncSources(nil): %v", err)
	}
}

func TestSyncSources_Serialization(t *testing.T) {
	db, _ := newMock(t)
	s := newSyncer(db, connectLocal)
	s.runMu.Lock()
	defer s.runMu.Unlock()
	if err := s.SyncSources(ctx, []string{"s1"}); err != ErrSyncInProgress {
		t.Errorf("SyncSources while locked = %v, want ErrSyncInProgress", err)
	}
}

func TestSyncSources_SourcesListError(t *testing.T) {
	db, mock := newMock(t)
	mock.ExpectQuery("SELECT .+ FROM state_sources ORDER BY").WillReturnError(errors.New("db down"))
	if err := newSyncer(db, connectLocal).SyncSources(ctx, []string{"s1"}); err == nil {
		t.Error("SyncSources: expected error when sources cannot be listed")
	}
}

func TestSyncKeyAndDropKey(t *testing.T) {
	db, mock := newMock(t)
	dir := t.TempDir()
	seed(t, dir, "app.tfstate", minState(7, "aws_instance"))

	mock.ExpectExec("INSERT INTO state_analyses").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO state_analysis_history").WillReturnResult(sqlmock.NewResult(0, 1))
	s := newSyncer(db, connectLocal)
	s.SyncKey(localSource(dir), "app.tfstate")

	mock.ExpectExec("DELETE FROM state_analyses WHERE source_id = .+ AND state_key").
		WithArgs("s1", "app.tfstate").WillReturnResult(sqlmock.NewResult(0, 1))
	s.DropKey(localSource(dir), "app.tfstate")

	// Errors are swallowed (logged): a failing refresh must not panic.
	s2 := newSyncer(db, func(*repositories.Source) (statesource.Connector, error) {
		return nil, errors.New("nope")
	})
	s2.SyncKey(localSource(dir), "app.tfstate")

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestStartStopLoop(t *testing.T) {
	db, mock := newMock(t)
	// The immediate boot cycle lists sources; give it an empty result.
	mock.ExpectQuery("SELECT .+ FROM state_sources ORDER BY").
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "type", "endpoint", "config", "scope", "encrypted_credentials", "created_at", "updated_at"}))
	s := newSyncer(db, connectLocal)
	s.interval = time.Hour
	s.Start()
	time.Sleep(50 * time.Millisecond) // let the boot cycle run
	s.Stop()
}

func TestMarker(t *testing.T) {
	now := time.Date(2026, 6, 11, 9, 0, 0, 0, time.UTC)
	cases := []struct {
		ref  statesource.StateRef
		want string
	}{
		{statesource.StateRef{Size: 2048, LastModified: &now}, "2048|2026-06-11T09:00:00Z"},
		{statesource.StateRef{Size: 0, LastModified: &now}, "0|2026-06-11T09:00:00Z"}, // HCP: updated-at only
		{statesource.StateRef{Size: 1024}, "1024|"},
		{statesource.StateRef{}, ""}, // no metadata -> always re-read
		// Version token disambiguates same-size changes (consul ModifyIndex,
		// pg content hash) and stands alone when size/timestamp are absent.
		{statesource.StateRef{Size: 1024, Version: "41"}, "1024||41"},
		{statesource.StateRef{Version: "d41d8cd9"}, "0||d41d8cd9"},
	}
	for i, c := range cases {
		if got := marker(c.ref); got != c.want {
			t.Errorf("case %d: marker = %q, want %q", i, got, c.want)
		}
	}
}

func TestAnalysisMarker(t *testing.T) {
	now := time.Date(2026, 6, 11, 9, 0, 0, 0, time.UTC)
	suffix := "|a" + strconv.Itoa(analyzer.AnalysisVersion)

	// A real change token gets the analyzer version folded in.
	if got, want := analysisMarker(statesource.StateRef{Size: 2048, LastModified: &now}), "2048|2026-06-11T09:00:00Z"+suffix; got != want {
		t.Errorf("analysisMarker(metadata) = %q, want %q", got, want)
	}
	// Marker-less backends keep the empty sentinel (always re-read), regardless
	// of the analyzer version.
	if got := analysisMarker(statesource.StateRef{}); got != "" {
		t.Errorf("analysisMarker(no metadata) = %q, want \"\"", got)
	}
}

// ---------------------------------------------------------------------------
// Backup retention (#257)
// ---------------------------------------------------------------------------

// The prune runs on the PERIODIC path only. SyncAll is also invoked on demand
// (post-write refresh, source-create backfill) on every replica including
// non-worker ones, so pruning there would take a destructive sweep outside the
// leader gate.
func TestBackupRetentionPrunesOnPeriodicCycleOnly(t *testing.T) {
	db, mock, err := newSQLMock()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	s := newSyncer(db, func(*repositories.Source) (statesource.Connector, error) {
		return &fakeConn{}, nil
	})
	s.EnableBackupRetention(repositories.NewStateEditRepository(db), 20, 90*24*time.Hour)

	// On-demand SyncAll: sources listing + history prune, but NO backup prune.
	mock.ExpectQuery("SELECT .+ FROM state_sources ORDER BY").
		WillReturnRows(sqlmock.NewRows([]string{"id"}))
	mock.ExpectExec("DELETE FROM state_analysis_history").
		WillReturnResult(sqlmock.NewResult(0, 0))
	if err := s.SyncAll(context.Background()); err != nil {
		t.Fatalf("SyncAll: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("on-demand SyncAll must not prune backups: %v", err)
	}

	// Periodic cycle: the backup prune follows the same cycle's history prune.
	mock.ExpectQuery("SELECT .+ FROM state_sources ORDER BY").
		WillReturnRows(sqlmock.NewRows([]string{"id"}))
	mock.ExpectExec("DELETE FROM state_analysis_history").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("DELETE FROM state_backups").
		WithArgs(20, 90*24*time.Hour.Seconds()).
		WillReturnResult(sqlmock.NewResult(0, 3))
	s.syncAllLogged()
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("periodic cycle must prune backups: %v", err)
	}
}

// With retention unconfigured (operator set backup_retention.enabled=false) the
// periodic cycle must issue no backup DELETE at all.
func TestBackupRetentionDisabledIssuesNoDelete(t *testing.T) {
	db, mock, err := newSQLMock()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	s := newSyncer(db, func(*repositories.Source) (statesource.Connector, error) {
		return &fakeConn{}, nil
	})

	mock.ExpectQuery("SELECT .+ FROM state_sources ORDER BY").
		WillReturnRows(sqlmock.NewRows([]string{"id"}))
	mock.ExpectExec("DELETE FROM state_analysis_history").
		WillReturnResult(sqlmock.NewResult(0, 0))
	s.syncAllLogged()
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("disabled retention must issue no backup DELETE: %v", err)
	}
}

// A failing prune must not abort the cycle — it is a bounded cleanup, not part
// of the reconcile contract.
func TestBackupRetentionErrorDoesNotFailCycle(t *testing.T) {
	db, mock, err := newSQLMock()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	s := newSyncer(db, func(*repositories.Source) (statesource.Connector, error) {
		return &fakeConn{}, nil
	})
	s.EnableBackupRetention(repositories.NewStateEditRepository(db), 20, time.Hour)

	mock.ExpectQuery("SELECT .+ FROM state_sources ORDER BY").
		WillReturnRows(sqlmock.NewRows([]string{"id"}))
	mock.ExpectExec("DELETE FROM state_analysis_history").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("DELETE FROM state_backups").
		WillReturnError(errors.New("db down"))
	s.syncAllLogged() // must not panic
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}
