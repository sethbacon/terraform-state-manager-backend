package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
	"github.com/lib/pq"
)

// newModulesRouter mounts the module-provenance read handlers bare (no auth
// middleware, mirroring newDriftEnv) over a sqlmock DB.
func newModulesRouter(t *testing.T) (*gin.Engine, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
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
		WithArgs(pq.Array([]string{"registry.terraform.io"}), "terraform-aws-modules/vpc/aws").
		WillReturnRows(sqlmock.NewRows(cols).AddRow("s1", "prod", "app.tfstate", nil, "2026-06-14"))

	w := httptest.NewRecorder()
	// Mixed-case + default port on the inbound host must be folded to the
	// canonical form before the join (and de-duplicated against itself).
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet,
		"/api/v1/consumers?host=Registry.Terraform.io:443&module=terraform-aws-modules/vpc/aws", nil))
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
