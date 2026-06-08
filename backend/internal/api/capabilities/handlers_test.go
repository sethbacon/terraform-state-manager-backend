package capabilities

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/terraform-state-manager/terraform-state-manager/internal/capability"
)

func init() {
	gin.SetMode(gin.TestMode)
}

type listResponse struct {
	Data []struct {
		Name     string   `json:"name"`
		Key      string   `json:"key"`
		TaskType string   `json:"task_type"`
		Scopes   []string `json:"scopes"`
	} `json:"data"`
	Total int `json:"total"`
}

func doList(t *testing.T, reg *capability.Registry) listResponse {
	t.Helper()
	h := NewHandlers(reg)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/capabilities", nil)

	h.ListCapabilities(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var resp listResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	return resp
}

func TestListCapabilities_Empty(t *testing.T) {
	resp := doList(t, capability.NewRegistry())
	if resp.Total != 0 || len(resp.Data) != 0 {
		t.Errorf("empty registry total = %d, len = %d, want 0/0", resp.Total, len(resp.Data))
	}
}

func TestListCapabilities_NilRegistry(t *testing.T) {
	resp := doList(t, nil)
	if resp.Total != 0 {
		t.Errorf("nil registry total = %d, want 0", resp.Total)
	}
}

func TestListCapabilities_ReportsRegistered(t *testing.T) {
	reg := capability.NewRegistry()
	reg.Register(capability.Capability{
		Name:     "Version No-Op Test",
		Key:      "versiontest",
		TaskType: "versiontest",
		Scopes:   []string{"versiontest:admin"},
	})

	resp := doList(t, reg)
	if resp.Total != 1 || len(resp.Data) != 1 {
		t.Fatalf("total = %d, len = %d, want 1/1", resp.Total, len(resp.Data))
	}
	got := resp.Data[0]
	if got.Key != "versiontest" || got.TaskType != "versiontest" {
		t.Errorf("got key=%q taskType=%q, want versiontest/versiontest", got.Key, got.TaskType)
	}
	if len(got.Scopes) != 1 || got.Scopes[0] != "versiontest:admin" {
		t.Errorf("scopes = %v, want [versiontest:admin]", got.Scopes)
	}
}
