package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

// newModulesRouter mounts the module-provenance read handlers bare (no auth
// middleware, mirroring newDriftEnv) over a sqlmock DB.
func newModulesRouter(t *testing.T) (*gin.Engine, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := newSQLMock()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	h := NewSourcesHandlers(db, nil)
	r := gin.New()
	v1 := r.Group("/api/v1")
	v1.GET("/sources/:id/modules", h.ListStateModules())
	v1.GET("/consumers", h.Consumers())
	return r, mock
}

func TestListStateModules(t *testing.T) {
	r, mock := newModulesRouter(t)
	cols := []string{"source_id", "state_key", "module_source", "module_version", "registry_host", "observed_at"}
	mock.ExpectQuery("FROM state_module_refs WHERE source_id").WithArgs("s1").
		WillReturnRows(sqlmock.NewRows(cols).
			AddRow("s1", "app.tfstate", "terraform-aws-modules/vpc/aws", nil, "registry.terraform.io", "2026-06-14"))

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/sources/s1/modules", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", w.Code, w.Body.String())
	}
	var resp struct {
		Modules []map[string]any `json:"modules"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Modules) != 1 || resp.Modules[0]["module_source"] != "terraform-aws-modules/vpc/aws" {
		t.Fatalf("unexpected modules: %+v", resp.Modules)
	}
}

func TestConsumers_MissingParamsIs400(t *testing.T) {
	r, _ := newModulesRouter(t)
	w := httptest.NewRecorder()
	// module is absent → 400 before any DB call.
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/consumers?host=registry.terraform.io", nil))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("missing module param must be 400, got %d (%s)", w.Code, w.Body.String())
	}
}

func TestConsumers_HostMatchedJoin(t *testing.T) {
	r, mock := newModulesRouter(t)
	cols := []string{"source_id", "source_name", "state_key", "module_version", "observed_at"}
	mock.ExpectQuery("WHERE r.registry_host_canon = ANY.+ AND r.module_source").
		WithArgs([]string{"registry.terraform.io"}, "terraform-aws-modules/vpc/aws").
		WillReturnRows(sqlmock.NewRows(cols).AddRow("s1", "prod", "app.tfstate", nil, "2026-06-14"))

	w := httptest.NewRecorder()
	// Mixed-case + default port on the inbound host must be folded to the
	// canonical form before the join (and de-duplicated against itself).
	// fleet=1 keeps this test about HOST canonicalization: it is the platform-admin
	// path, which delegates to the same unscoped query, so the join being asserted
	// is unchanged by #439's tenancy work.
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet,
		"/api/v1/consumers?host=Registry.Terraform.io:443&module=terraform-aws-modules/vpc/aws&fleet=1", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", w.Code, w.Body.String())
	}
	var resp struct {
		Consumers []map[string]any `json:"consumers"`
		Total     int              `json:"total"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Total != 1 || len(resp.Consumers) != 1 || resp.Consumers[0]["source_name"] != "prod" {
		t.Fatalf("unexpected consumers: %+v (total %d)", resp.Consumers, resp.Total)
	}
}

// ---------------------------------------------------------------------------
// #439 — /consumers has no principal, so the sibling registry must declare the
// tenancy on the caller's behalf, and a request declaring none is refused.
//
// Refusing is what makes the disclosure closable. Reading a MISSING parameter
// as "fleet-wide" cannot be told apart from a caller that simply did not send
// one, so it would hand every organization's state topology to anything that
// omitted it.
// ---------------------------------------------------------------------------

func TestConsumers_NoTenancyDeclaredIsRefused(t *testing.T) {
	r, _ := newModulesRouter(t)

	// No query is staged. That is the assertion: the refusal must happen before
	// the database is touched at all, so an un-scoped read cannot occur even
	// momentarily.
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet,
		"/api/v1/consumers?host=registry.terraform.io&module=acme/vpc/aws", nil))

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (%s)", w.Code, w.Body.String())
	}
}

func TestConsumers_OrganizationScopesTheJoin(t *testing.T) {
	r, mock := newModulesRouter(t)
	cols := []string{"source_id", "source_name", "state_key", "module_version", "observed_at"}
	// The scoped query carries the organization predicate as a THIRD argument,
	// joined through state_sources -- state_module_refs has no organization_id.
	mock.ExpectQuery("JOIN state_sources s ON s.id = r.source_id AND s.organization_id").
		WithArgs([]string{"registry.terraform.io"}, "acme/vpc/aws",
			[]string{"11111111-1111-1111-1111-111111111111"}).
		WillReturnRows(sqlmock.NewRows(cols).AddRow("s1", "prod", "app.tfstate", nil, "2026-06-14"))

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet,
		"/api/v1/consumers?host=registry.terraform.io&module=acme/vpc/aws"+
			"&organization=11111111-1111-1111-1111-111111111111", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// A malformed organization must be a 400, not a 500: `= ANY($3::uuid[])` on a
// non-UUID raises Postgres 22P02.
func TestConsumers_MalformedOrganizationIsABadRequest(t *testing.T) {
	r, _ := newModulesRouter(t)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet,
		"/api/v1/consumers?host=registry.terraform.io&module=acme/vpc/aws&organization=not-a-uuid", nil))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (%s)", w.Code, w.Body.String())
	}
}

// The two declarations are mutually exclusive; refusing beats guessing which
// one the sibling meant.
func TestConsumers_FleetAndOrganizationTogetherIsRefused(t *testing.T) {
	r, _ := newModulesRouter(t)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet,
		"/api/v1/consumers?host=registry.terraform.io&module=acme/vpc/aws"+
			"&organization=11111111-1111-1111-1111-111111111111&fleet=1", nil))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (%s)", w.Code, w.Body.String())
	}
}
