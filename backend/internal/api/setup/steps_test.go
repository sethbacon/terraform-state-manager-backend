package setup

import (
	"net/http"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"

	"github.com/terraform-state-manager/terraform-state-manager/internal/config"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
)

func TestConfigureAdmin_CoupledReturns409(t *testing.T) {
	cfg := &config.Config{}
	cfg.Suite.RoleSeedOwner = "registry" // coupled — sibling owns identity
	h := NewHandlers(nil, nil, nil, nil, cfg, nil)
	if w := postJSON(h.ConfigureAdmin, `{"email":"owner@example.com"}`); w.Code != http.StatusConflict {
		t.Fatalf("coupled ConfigureAdmin = %d, want 409 (%s)", w.Code, w.Body.String())
	}
}

func TestConfigureAdmin_BadEmailReturns400(t *testing.T) {
	cfg := &config.Config{}
	cfg.Suite.RoleSeedOwner = "self"
	h := NewHandlers(nil, nil, nil, nil, cfg, nil)
	if w := postJSON(h.ConfigureAdmin, `{"email":"not-an-email"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("invalid email ConfigureAdmin = %d, want 400", w.Code)
	}
}

// settingsMock returns a SystemSettingsRepository whose GetStatus reports the
// given flags; when expectComplete it also expects the SetSetupCompleted UPDATE.
func settingsMock(t *testing.T, admin, oidc, sources, expectComplete bool) *repositories.SystemSettingsRepository {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	cols := []string{"setup_completed", "admin_configured", "oidc_configured", "ldap_configured", "sources_configured", "auth_method"}
	mock.ExpectQuery("FROM system_settings WHERE id = 1").
		WillReturnRows(sqlmock.NewRows(cols).AddRow(false, admin, oidc, false, sources, nil))
	if expectComplete {
		mock.ExpectExec("UPDATE system_settings SET setup_completed = true").WillReturnResult(sqlmock.NewResult(0, 1))
	}
	return repositories.NewSystemSettingsRepository(db)
}

// Standalone with owner + OIDC but no source: CompleteSetup must refuse.
func TestCompleteSetup_MissingSourceReturns400(t *testing.T) {
	cfg := &config.Config{}
	cfg.Suite.RoleSeedOwner = "self"
	h := NewHandlers(settingsMock(t, true, true, false, false), nil, nil, nil, cfg, nil)
	if w := postJSON(h.CompleteSetup, ``); w.Code != http.StatusBadRequest {
		t.Fatalf("no source CompleteSetup = %d, want 400 (%s)", w.Code, w.Body.String())
	}
}

// All prerequisites met: CompleteSetup succeeds and burns the token.
func TestCompleteSetup_AllSatisfiedReturns200(t *testing.T) {
	cfg := &config.Config{}
	cfg.Suite.RoleSeedOwner = "self"
	h := NewHandlers(settingsMock(t, true, true, true, true), nil, nil, nil, cfg, nil)
	if w := postJSON(h.CompleteSetup, ``); w.Code != http.StatusOK {
		t.Fatalf("satisfied CompleteSetup = %d, want 200 (%s)", w.Code, w.Body.String())
	}
}

func TestSaveSource_MissingFieldsReturns400(t *testing.T) {
	h := NewHandlers(nil, nil, nil, nil, &config.Config{}, nil)
	if w := postJSON(h.SaveSource, `{}`); w.Code != http.StatusBadRequest {
		t.Fatalf("SaveSource missing fields = %d, want 400", w.Code)
	}
}

func TestTestSource_MissingFieldsReturns400(t *testing.T) {
	h := NewHandlers(nil, nil, nil, nil, &config.Config{}, nil)
	if w := postJSON(h.TestSource, `{}`); w.Code != http.StatusBadRequest {
		t.Fatalf("TestSource missing fields = %d, want 400", w.Code)
	}
}
