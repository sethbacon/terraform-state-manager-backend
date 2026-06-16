package setup

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"

	"github.com/terraform-state-manager/terraform-state-manager/internal/config"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
)

// statusBody renders GET /setup/status for a given role_seed_owner and returns
// the decoded JSON. system_settings is mocked as an un-completed singleton row.
func statusBody(t *testing.T, roleSeedOwner string) map[string]any {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	cols := []string{"setup_completed", "admin_configured", "oidc_configured", "ldap_configured", "sources_configured", "auth_method"}
	mock.ExpectQuery("FROM system_settings WHERE id = 1").
		WillReturnRows(sqlmock.NewRows(cols).AddRow(false, false, false, false, false, nil))

	cfg := &config.Config{}
	cfg.Suite.RoleSeedOwner = roleSeedOwner
	h := NewHandlers(repositories.NewSystemSettingsRepository(db), nil, nil, nil, cfg, nil)

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/setup/status", h.GetSetupStatus)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/setup/status", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", w.Code, w.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// Standalone (role_seed_owner=self): TSM owns its identity → the wizard runs the
// full flow, so identity_owned_externally is false.
func TestGetSetupStatus_StandaloneOwnsIdentity(t *testing.T) {
	out := statusBody(t, "self")
	if out["identity_owned_externally"] != false {
		t.Errorf("standalone must own identity, got %v", out["identity_owned_externally"])
	}
	if out["setup_required"] != true || out["setup_completed"] != false {
		t.Errorf("not-completed should report setup_required=true: %v", out)
	}
}

// Coupled (role_seed_owner=registry): the sibling owns identity → the wizard
// must defer the identity steps, so identity_owned_externally is true. This is
// the synchronous, boot-time signal (not live discovery) that prevents clobber.
func TestGetSetupStatus_CoupledDefersIdentity(t *testing.T) {
	out := statusBody(t, "registry")
	if out["identity_owned_externally"] != true {
		t.Errorf("coupled must defer identity, got %v", out["identity_owned_externally"])
	}
}

func TestValidateToken_OK(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	h := NewHandlers(nil, nil, nil, nil, &config.Config{}, nil)
	r.POST("/setup/validate-token", h.ValidateToken)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/setup/validate-token", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("validate-token = %d, want 200", w.Code)
	}
}
