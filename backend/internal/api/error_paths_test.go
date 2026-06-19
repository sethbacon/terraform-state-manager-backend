package api

import (
	"net/http"
	"strings"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
)

func sqlmockRowsForSource(cfgJSON string) *sqlmock.Rows {
	return sqlmock.NewRows([]string{"id", "name", "type", "endpoint", "config", "scope", "encrypted_credentials", "created_at", "updated_at"}).
		AddRow("s1", "hcp", "hcp", "", []byte(cfgJSON), []byte(`{}`), []byte("sealed"), "2026-06-11", "2026-06-11")
}

func stringsContains(s, sub string) bool { return strings.Contains(s, sub) }

// These sweeps pin the repository-failure contract for every read/write
// handler: a dead DB yields a clean 500 (or the handler's documented status),
// never a panic or a hung request. Each env queues NO sqlmock expectations, so
// every query errors.

func TestErrorPaths_Sources(t *testing.T) {
	e := newSourcesEnv(t)
	cases := []struct {
		method, path, body string
		want               int
	}{
		{http.MethodGet, "/api/v1/sources/s1", "", http.StatusInternalServerError},
		{http.MethodDelete, "/api/v1/sources/s1", "", http.StatusInternalServerError},
		{http.MethodGet, "/api/v1/sources/s1/states", "", http.StatusInternalServerError},
		{http.MethodGet, "/api/v1/sources/s1/state/analysis?key=k", "", http.StatusInternalServerError},
		{http.MethodGet, "/api/v1/sources/s1/state/backups?key=k", "", http.StatusInternalServerError},
		{http.MethodPost, "/api/v1/sources/s1/state/backups/b1/restore", "", http.StatusInternalServerError},
		{http.MethodPost, "/api/v1/sources/s1/state/backup?key=k", `{"target_source_id":"s2","target_key":"t"}`, http.StatusInternalServerError},
	}
	for _, tc := range cases {
		if w := e.do(tc.method, tc.path, tc.body); w.Code != tc.want {
			t.Errorf("%s %s: status = %d, want %d (%s)", tc.method, tc.path, w.Code, tc.want, w.Body.String())
		}
	}
}

func TestErrorPaths_DriftHealthSchedules(t *testing.T) {
	e := newDriftEnv(t)
	for _, tc := range []struct {
		method, path, body string
		want               int
	}{
		{http.MethodGet, "/api/v1/pipelines", "", http.StatusInternalServerError},
		{http.MethodPost, "/api/v1/pipelines", `{"name":"x","provider":"github_actions"}`, http.StatusInternalServerError},
		{http.MethodPut, "/api/v1/pipelines/p1", `{"name":"x"}`, http.StatusInternalServerError},
		{http.MethodDelete, "/api/v1/pipelines/p1", "", http.StatusInternalServerError},
		{http.MethodGet, "/api/v1/drift/runs", "", http.StatusInternalServerError},
		{http.MethodGet, "/api/v1/drift/runs/d1", "", http.StatusInternalServerError},
		{http.MethodPost, "/api/v1/drift/runs", `{"pipeline_connection_id":"p1"}`, http.StatusInternalServerError},
		{http.MethodPost, "/api/v1/drift/runs/d1/results", `{"token":"t"}`, http.StatusInternalServerError},
		{http.MethodGet, "/api/v1/health-lab/runs", "", http.StatusInternalServerError},
		{http.MethodGet, "/api/v1/health-lab/runs/h1", "", http.StatusInternalServerError},
		{http.MethodPost, "/api/v1/health-lab/runs", `{"pipeline_connection_id":"p1"}`, http.StatusInternalServerError},
	} {
		if w := e.do(tc.method, tc.path, tc.body); w.Code != tc.want {
			t.Errorf("%s %s: status = %d, want %d (%s)", tc.method, tc.path, w.Code, tc.want, w.Body.String())
		}
	}

	s := newSchedulesEnv(t)
	for _, tc := range []struct {
		method, path, body string
		want               int
	}{
		{http.MethodGet, "/api/v1/schedules", "", http.StatusInternalServerError},
		{http.MethodPost, "/api/v1/schedules", `{"name":"n","cron_expr":"daily","target_config":{"pipeline_connection_id":"p1"}}`, http.StatusInternalServerError},
		{http.MethodGet, "/api/v1/schedules/sc1", "", http.StatusInternalServerError},
		{http.MethodPut, "/api/v1/schedules/sc1", `{"name":"n","cron_expr":"daily","target_config":{"pipeline_connection_id":"p1"}}`, http.StatusInternalServerError},
		{http.MethodDelete, "/api/v1/schedules/sc1", "", http.StatusInternalServerError},
		{http.MethodPost, "/api/v1/schedules/sc1/run", "", http.StatusInternalServerError},
	} {
		if w := s.do(tc.method, tc.path, tc.body); w.Code != tc.want {
			t.Errorf("%s %s: status = %d, want %d (%s)", tc.method, tc.path, w.Code, tc.want, w.Body.String())
		}
	}
}

func TestErrorPaths_NotificationsAndCISources(t *testing.T) {
	n := newNotificationsEnv(t)
	for _, tc := range []struct {
		method, path, body string
		want               int
	}{
		{http.MethodGet, "/api/v1/notifications/channels", "", http.StatusInternalServerError},
		{http.MethodPost, "/api/v1/notifications/channels", `{"name":"n","type":"slack","target":"https://h.example/x"}`, http.StatusInternalServerError},
		{http.MethodPut, "/api/v1/notifications/channels/n1", `{"name":"n","type":"slack"}`, http.StatusInternalServerError},
		{http.MethodDelete, "/api/v1/notifications/channels/n1", "", http.StatusInternalServerError},
		{http.MethodPost, "/api/v1/notifications/channels/n1/test", "", http.StatusBadGateway},
	} {
		if w := n.do(tc.method, tc.path, tc.body); w.Code != tc.want {
			t.Errorf("%s %s: status = %d, want %d (%s)", tc.method, tc.path, w.Code, tc.want, w.Body.String())
		}
	}

	c := newCISourcesEnv(t)
	for _, tc := range []struct {
		method, path, body string
		want               int
	}{
		{http.MethodGet, "/api/v1/ci-sources", "", http.StatusInternalServerError},
		{http.MethodPost, "/api/v1/ci-sources", `{"name":"n","provider":"github_actions","organization":"o","token":"t"}`, http.StatusInternalServerError},
		{http.MethodDelete, "/api/v1/ci-sources/c1", "", http.StatusInternalServerError},
		{http.MethodGet, "/api/v1/ci-sources/c1/pipelines", "", http.StatusInternalServerError},
	} {
		if w := c.do(tc.method, tc.path, tc.body); w.Code != tc.want {
			t.Errorf("%s %s: status = %d, want %d (%s)", tc.method, tc.path, w.Code, tc.want, w.Body.String())
		}
	}
}

func TestErrorPaths_AdminWrite(t *testing.T) {
	e := newAdminWriteEnv(t)
	for _, tc := range []struct {
		method, path, body string
		want               int
	}{
		{http.MethodPost, "/api/v1/admin/users", `{"email":"a@b.c"}`, http.StatusInternalServerError},
		{http.MethodPut, "/api/v1/admin/users/u1", `{"name":"X"}`, http.StatusNotFound},
		{http.MethodDelete, "/api/v1/admin/users/u1", "", http.StatusInternalServerError},
		{http.MethodGet, "/api/v1/admin/users/u1/export", "", http.StatusNotFound},
		{http.MethodPost, "/api/v1/admin/users/u1/erase", "", http.StatusNotFound},
		{http.MethodPost, "/api/v1/admin/organizations", `{"name":"n"}`, http.StatusInternalServerError},
		{http.MethodPut, "/api/v1/admin/organizations/o1", `{"name":"n"}`, http.StatusNotFound},
		{http.MethodDelete, "/api/v1/admin/organizations/o1", "", http.StatusInternalServerError},
		{http.MethodGet, "/api/v1/admin/organizations/o1/members", "", http.StatusInternalServerError},
		{http.MethodPost, "/api/v1/admin/organizations/o1/members", `{"user_id":"u1"}`, http.StatusInternalServerError},
		{http.MethodPut, "/api/v1/admin/organizations/o1/members/u1", `{}`, http.StatusInternalServerError},
		{http.MethodDelete, "/api/v1/admin/organizations/o1/members/u1", "", http.StatusInternalServerError},
	} {
		if w := e.do(tc.method, tc.path, tc.body); w.Code != tc.want {
			t.Errorf("%s %s: status = %d, want %d (%s)", tc.method, tc.path, w.Code, tc.want, w.Body.String())
		}
	}
}

func TestCreateSource_WithEncryptedCredentials(t *testing.T) {
	e := newSourcesEnv(t)
	cfgJSON := `{"organization":"acme"}`
	e.mock.ExpectQuery("INSERT INTO state_sources").
		WillReturnRows(sqlmockRowsForSource(cfgJSON))
	body := `{"name":"hcp","type":"hcp","config":{"organization":"acme"},"credentials":{"token":"tfe-secret"}}`
	w := e.do(http.MethodPost, "/api/v1/sources", body)
	if w.Code != http.StatusCreated {
		t.Fatalf("create with credentials: status = %d (%s)", w.Code, w.Body.String())
	}
	if stringsContains(w.Body.String(), "tfe-secret") {
		t.Error("response leaked the credential")
	}
}
