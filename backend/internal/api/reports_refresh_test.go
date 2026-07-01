package api

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"

	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
	"github.com/terraform-state-manager/terraform-state-manager/internal/services/statesync"
	"github.com/terraform-state-manager/terraform-state-manager/internal/statesource"
)

// refreshEnv wires the Reports route over a syncer-attached handler with a real
// local connector, recording which sources the reconcile connected so a test can
// assert the refresh scoped correctly.
type refreshEnv struct {
	r         *gin.Engine
	mock      sqlmock.Sqlmock
	connected map[string]bool
	mu        sync.Mutex
}

func newRefreshEnv(t *testing.T) *refreshEnv {
	t.Helper()
	t.Setenv("TSM_ENCRYPTION_KEY", "0123456789abcdef0123456789abcdef")
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	// The reconcile reads states concurrently with no fixed query order.
	mock.MatchExpectationsInOrder(false)

	env := &refreshEnv{mock: mock, connected: map[string]bool{}}
	connect := func(s *repositories.Source) (statesource.Connector, error) {
		env.mu.Lock()
		env.connected[s.ID] = true
		env.mu.Unlock()
		return ConnectSource(s)
	}

	h := NewSourcesHandlers(db, nil)
	h.AttachSyncer(statesync.New(
		repositories.NewSourceRepository(db),
		repositories.NewStateAnalysisRepository(db),
		connect,
	))
	r := gin.New()
	r.GET("/api/v1/reports/states", h.ReportStates())
	env.r = r
	return env
}

func localSourceRows(rows ...[2]string) *sqlmock.Rows {
	cols := []string{"id", "name", "type", "endpoint", "config", "scope", "encrypted_credentials", "created_at", "updated_at"}
	out := sqlmock.NewRows(cols)
	for _, r := range rows {
		cfg, _ := json.Marshal(map[string]any{"base_path": r[1]})
		out.AddRow(r[0], r[0], "local", "", cfg, []byte(`{}`), nil, "2026-06-10", "2026-06-10")
	}
	return out
}

func TestReportStates_Refresh(t *testing.T) {
	t.Run("scoped to the selected source", func(t *testing.T) {
		env := newRefreshEnv(t)
		dir1, dir2 := t.TempDir(), t.TempDir()
		if err := os.WriteFile(filepath.Join(dir1, "app.tfstate"),
			[]byte(minState(7, "lin-1", "aws_instance.web")), 0o600); err != nil {
			t.Fatal(err)
		}

		// Two sources configured; only s1 is selected. The reconcile must scope to
		// s1 (markers queried for s1 only) and never connect s2.
		env.mock.ExpectQuery("SELECT .+ FROM state_sources ORDER BY").
			WillReturnRows(localSourceRows([2]string{"s1", dir1}, [2]string{"s2", dir2}))
		env.mock.ExpectQuery("SELECT state_key, version_marker FROM state_analyses").WithArgs("s1").
			WillReturnRows(sqlmock.NewRows([]string{"state_key", "version_marker"}))
		env.mock.ExpectExec("INSERT INTO state_analyses").WillReturnResult(sqlmock.NewResult(0, 1))
		env.mock.ExpectExec("INSERT INTO state_analysis_history").WillReturnResult(sqlmock.NewResult(0, 1))
		env.mock.ExpectExec("DELETE FROM state_analyses WHERE source_id").WillReturnResult(sqlmock.NewResult(0, 0))
		env.mock.ExpectExec("INSERT INTO source_sync_status").WillReturnResult(sqlmock.NewResult(0, 1))
		// Then the report read aggregates the store.
		env.mock.ExpectQuery("FROM state_analyses a").WillReturnRows(allStatesRows())

		w := doGet(env.r, "/api/v1/reports/states?refresh=true&source_id=s1")
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d (%s)", w.Code, w.Body.String())
		}
		if !env.connected["s1"] {
			t.Error("s1 should have been reconciled")
		}
		if env.connected["s2"] {
			t.Error("s2 was reconciled but was not the selected source")
		}
		if err := env.mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet expectations: %v", err)
		}
	})

	t.Run("unscoped refreshes the whole fleet", func(t *testing.T) {
		env := newRefreshEnv(t)
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "app.tfstate"),
			[]byte(minState(7, "lin-1", "aws_instance.web")), 0o600); err != nil {
			t.Fatal(err)
		}

		// No source filter -> a full cycle, which also prunes history.
		env.mock.ExpectQuery("SELECT .+ FROM state_sources ORDER BY").
			WillReturnRows(localSourceRows([2]string{"s1", dir}))
		env.mock.ExpectQuery("SELECT state_key, version_marker FROM state_analyses").WithArgs("s1").
			WillReturnRows(sqlmock.NewRows([]string{"state_key", "version_marker"}))
		env.mock.ExpectExec("INSERT INTO state_analyses").WillReturnResult(sqlmock.NewResult(0, 1))
		env.mock.ExpectExec("INSERT INTO state_analysis_history").WillReturnResult(sqlmock.NewResult(0, 1))
		env.mock.ExpectExec("DELETE FROM state_analyses WHERE source_id").WillReturnResult(sqlmock.NewResult(0, 0))
		env.mock.ExpectExec("DELETE FROM state_analysis_history").WillReturnResult(sqlmock.NewResult(0, 0))
		env.mock.ExpectExec("INSERT INTO source_sync_status").WillReturnResult(sqlmock.NewResult(0, 1))
		env.mock.ExpectQuery("FROM state_analyses a").WillReturnRows(allStatesRows())

		w := doGet(env.r, "/api/v1/reports/states?refresh=true")
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d (%s)", w.Code, w.Body.String())
		}
		if !env.connected["s1"] {
			t.Error("s1 should have been reconciled by the full cycle")
		}
		if err := env.mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet expectations: %v", err)
		}
	})
}
