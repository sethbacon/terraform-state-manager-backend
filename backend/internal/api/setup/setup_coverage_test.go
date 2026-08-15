package setup

import (
	"errors"
	"net/http"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"

	"github.com/terraform-state-manager/terraform-state-manager/internal/config"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
)

// ---------------------------------------------------------------------------
// Pure helpers (oidc.go)
// ---------------------------------------------------------------------------

func TestDefaultRedirectURL(t *testing.T) {
	cfg := &config.Config{}
	cfg.Server.PublicURL = "https://tsm.example.com/"
	h := NewHandlers(nil, nil, nil, nil, nil, nil, cfg, nil)
	if got := h.defaultRedirectURL(); got != "https://tsm.example.com/api/v1/auth/callback" {
		t.Errorf("PublicURL: got %q", got)
	}

	// Falls back to BaseURL when PublicURL is empty.
	cfg2 := &config.Config{}
	cfg2.Server.BaseURL = "https://base.example.com"
	h2 := NewHandlers(nil, nil, nil, nil, nil, nil, cfg2, nil)
	if got := h2.defaultRedirectURL(); got != "https://base.example.com/api/v1/auth/callback" {
		t.Errorf("BaseURL fallback: got %q", got)
	}
}

func TestOIDCRequestToConfig(t *testing.T) {
	// Empty scopes/redirect fall back to the defaults.
	c := oidcRequest{IssuerURL: "https://idp", ClientID: "id", ClientSecret: "sec"}.toConfig("https://def/cb")
	if !c.Enabled || c.IssuerURL != "https://idp" || c.ClientID != "id" || c.ClientSecret != "sec" {
		t.Errorf("config not populated: %+v", c)
	}
	if c.RedirectURL != "https://def/cb" {
		t.Errorf("redirect default: %q", c.RedirectURL)
	}
	if len(c.Scopes) != 3 {
		t.Errorf("scopes default: %v", c.Scopes)
	}

	// Explicit scopes/redirect are honored.
	c2 := oidcRequest{
		IssuerURL: "https://idp", ClientID: "id", ClientSecret: "sec",
		RedirectURL: "https://cust/cb", Scopes: []string{"openid"},
	}.toConfig("https://def/cb")
	if c2.RedirectURL != "https://cust/cb" || len(c2.Scopes) != 1 {
		t.Errorf("custom values not honored: %+v", c2)
	}
}

// ---------------------------------------------------------------------------
// ConfigureAdmin guards (admin.go)
// ---------------------------------------------------------------------------

func TestConfigureAdmin_Coupled409(t *testing.T) {
	cfg := &config.Config{}
	cfg.Suite.RoleSeedOwner = "registry" // coupled: the sibling registry owns identity
	h := NewHandlers(nil, nil, nil, nil, nil, nil, cfg, nil)
	if w := postJSON(h.ConfigureAdmin, `{"email":"owner@example.com"}`); w.Code != http.StatusConflict {
		t.Errorf("coupled ConfigureAdmin = %d, want 409 (%s)", w.Code, w.Body.String())
	}
}

func TestConfigureAdmin_BadEmail400(t *testing.T) {
	cfg := &config.Config{}
	cfg.Suite.RoleSeedOwner = "self"
	h := NewHandlers(nil, nil, nil, nil, nil, nil, cfg, nil)
	if w := postJSON(h.ConfigureAdmin, `{"email":"not-an-email"}`); w.Code != http.StatusBadRequest {
		t.Errorf("bad-email ConfigureAdmin = %d, want 400", w.Code)
	}
}

// ---------------------------------------------------------------------------
// Source guards (sources.go)
// ---------------------------------------------------------------------------

func TestSaveSource_BadInput(t *testing.T) {
	h := NewHandlers(nil, nil, nil, nil, nil, nil, &config.Config{}, nil)
	if w := postJSON(h.SaveSource, `{bad`); w.Code != http.StatusBadRequest {
		t.Errorf("bad-json SaveSource = %d, want 400", w.Code)
	}
	if w := postJSON(h.SaveSource, `{"name":"s","type":"nonexistent-connector-xyz"}`); w.Code != http.StatusBadRequest {
		t.Errorf("unknown-type SaveSource = %d, want 400 (%s)", w.Code, w.Body.String())
	}
}

func TestTestSource_BadInput(t *testing.T) {
	h := NewHandlers(nil, nil, nil, nil, nil, nil, &config.Config{}, nil)
	if w := postJSON(h.TestSource, `{bad`); w.Code != http.StatusBadRequest {
		t.Errorf("bad-json TestSource = %d, want 400", w.Code)
	}
	if w := postJSON(h.TestSource, `{"name":"s","type":"nonexistent-connector-xyz"}`); w.Code != http.StatusBadRequest {
		t.Errorf("unknown-type TestSource = %d, want 400 (%s)", w.Code, w.Body.String())
	}
}

// ---------------------------------------------------------------------------
// CompleteSetup (admin.go) over a sqlmock-backed system_settings repo
// ---------------------------------------------------------------------------

var setupStatusCols = []string{
	"setup_completed", "admin_configured", "oidc_configured",
	"ldap_configured", "sources_configured", "auth_method",
}

func newCompleteSetupHandler(t *testing.T, roleSeedOwner string) (*Handlers, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	cfg := &config.Config{}
	cfg.Suite.RoleSeedOwner = roleSeedOwner
	h := NewHandlers(repositories.NewSystemSettingsRepository(db), nil, nil, nil, nil, nil, cfg, nil)
	return h, mock
}

func TestCompleteSetup_StatusError(t *testing.T) {
	h, mock := newCompleteSetupHandler(t, "self")
	mock.ExpectQuery("FROM system_settings WHERE id = 1").WillReturnError(errors.New("boom"))
	if w := postJSON(h.CompleteSetup, ""); w.Code != http.StatusInternalServerError {
		t.Errorf("status error = %d, want 500", w.Code)
	}
}

func TestCompleteSetup_StandaloneMissingPrereqs(t *testing.T) {
	// No owner configured yet.
	h, mock := newCompleteSetupHandler(t, "self")
	mock.ExpectQuery("FROM system_settings WHERE id = 1").
		WillReturnRows(sqlmock.NewRows(setupStatusCols).AddRow(false, false, false, false, false, nil))
	if w := postJSON(h.CompleteSetup, ""); w.Code != http.StatusBadRequest {
		t.Errorf("missing admin = %d, want 400", w.Code)
	}

	// Owner set, but no authentication method.
	h2, mock2 := newCompleteSetupHandler(t, "self")
	mock2.ExpectQuery("FROM system_settings WHERE id = 1").
		WillReturnRows(sqlmock.NewRows(setupStatusCols).AddRow(false, true, false, false, false, nil))
	if w := postJSON(h2.CompleteSetup, ""); w.Code != http.StatusBadRequest {
		t.Errorf("missing auth = %d, want 400", w.Code)
	}

	// Owner + OIDC, but no state source.
	h3, mock3 := newCompleteSetupHandler(t, "self")
	mock3.ExpectQuery("FROM system_settings WHERE id = 1").
		WillReturnRows(sqlmock.NewRows(setupStatusCols).AddRow(false, true, true, false, false, "oidc"))
	if w := postJSON(h3.CompleteSetup, ""); w.Code != http.StatusBadRequest {
		t.Errorf("missing sources = %d, want 400", w.Code)
	}
}

func TestCompleteSetup_StandaloneSuccess(t *testing.T) {
	h, mock := newCompleteSetupHandler(t, "self")
	mock.ExpectQuery("FROM system_settings WHERE id = 1").
		WillReturnRows(sqlmock.NewRows(setupStatusCols).AddRow(false, true, true, false, true, "oidc"))
	mock.ExpectExec("UPDATE system_settings SET setup_completed").WillReturnResult(sqlmock.NewResult(0, 1))
	if w := postJSON(h.CompleteSetup, ""); w.Code != http.StatusOK {
		t.Errorf("success = %d, want 200 (%s)", w.Code, w.Body.String())
	}
}

func TestCompleteSetup_CoupledOnlyNeedsSources(t *testing.T) {
	// Coupled mode skips the identity prerequisites (the sibling owns them); only
	// a state source is required.
	h, mock := newCompleteSetupHandler(t, "registry")
	mock.ExpectQuery("FROM system_settings WHERE id = 1").
		WillReturnRows(sqlmock.NewRows(setupStatusCols).AddRow(false, false, false, false, true, nil))
	mock.ExpectExec("UPDATE system_settings SET setup_completed").WillReturnResult(sqlmock.NewResult(0, 1))
	if w := postJSON(h.CompleteSetup, ""); w.Code != http.StatusOK {
		t.Errorf("coupled success = %d, want 200 (%s)", w.Code, w.Body.String())
	}
}
