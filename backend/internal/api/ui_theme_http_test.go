package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"

	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
)

func newThemeEnv(t *testing.T) (*gin.Engine, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	settings := repositories.NewSystemSettingsRepository(db)
	r := gin.New()
	r.GET("/api/v1/ui/theme", GetUITheme(settings))
	r.PUT("/api/v1/admin/ui/theme", UpdateUITheme(settings, newAuditor(nil)))
	return r, mock
}

func themeDo(r *gin.Engine, method, path, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	return w
}

func TestUITheme_GetEmptyAndStored(t *testing.T) {
	r, mock := newThemeEnv(t)

	// Never configured -> {} so the SPA falls back to built-in branding.
	mock.ExpectQuery("SELECT ui_theme_config FROM system_settings").
		WillReturnRows(sqlmock.NewRows([]string{"ui_theme_config"}).AddRow(nil))
	w := themeDo(r, http.MethodGet, "/api/v1/ui/theme", "")
	if w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != "{}" {
		t.Fatalf("empty theme: %d %s", w.Code, w.Body.String())
	}

	// Stored branding is served verbatim.
	stored := `{"product_name":"Acme State","primary_color":"#0a6e31"}`
	mock.ExpectQuery("SELECT ui_theme_config FROM system_settings").
		WillReturnRows(sqlmock.NewRows([]string{"ui_theme_config"}).AddRow([]byte(stored)))
	w = themeDo(r, http.MethodGet, "/api/v1/ui/theme", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Acme State") {
		t.Fatalf("stored theme: %d %s", w.Code, w.Body.String())
	}
}

func TestUITheme_GetNilSettingsRig(t *testing.T) {
	r := gin.New()
	r.GET("/api/v1/ui/theme", GetUITheme(nil))
	if w := themeDo(r, http.MethodGet, "/api/v1/ui/theme", ""); w.Code != http.StatusOK {
		t.Fatalf("nil-DB rig must serve {}: %d", w.Code)
	}
}

func TestUITheme_UpdatePersistsValidConfig(t *testing.T) {
	r, mock := newThemeEnv(t)

	mock.ExpectExec("UPDATE system_settings SET ui_theme_config").
		WillReturnResult(sqlmock.NewResult(0, 1))
	body := `{"product_name":"Acme State","primary_color":"#0a6e31","secondary_color_light":"rgb(10, 20, 30)","logo_url":"https://cdn.acme.example/logo.svg","favicon_url":"/branding/favicon.ico"}`
	w := themeDo(r, http.MethodPut, "/api/v1/admin/ui/theme", body)
	if w.Code != http.StatusOK {
		t.Fatalf("update: %d %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("theme not persisted: %v", err)
	}
}

func TestUITheme_UpdateRejectsBadValues(t *testing.T) {
	r, _ := newThemeEnv(t)

	cases := []struct{ name, body string }{
		{"unparsable color", `{"primary_color":"reddish"}`},
		{"css-injection color", `{"primary_color":"#fff;background:url(x)"}`},
		{"javascript url", `{"logo_url":"javascript:alert(1)"}`},
		{"plain http url", `{"logo_url":"http://cdn.example.com/logo.png"}`},
		{"protocol-relative url", `{"favicon_url":"//evil.example/x.ico"}`},
		{"overlong name", `{"product_name":"` + strings.Repeat("x", 101) + `"}`},
		{"not json", `nope`},
	}
	for _, tc := range cases {
		if w := themeDo(r, http.MethodPut, "/api/v1/admin/ui/theme", tc.body); w.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400 (%s)", tc.name, w.Code, w.Body.String())
		}
	}
}

func TestUITheme_UpdateEmptyClearsOverrides(t *testing.T) {
	r, mock := newThemeEnv(t)
	mock.ExpectExec("UPDATE system_settings SET ui_theme_config").
		WillReturnResult(sqlmock.NewResult(0, 1))
	if w := themeDo(r, http.MethodPut, "/api/v1/admin/ui/theme", `{}`); w.Code != http.StatusOK {
		t.Fatalf("clearing update: %d %s", w.Code, w.Body.String())
	}
}
