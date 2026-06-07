package uitheme

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/terraform-state-manager/terraform-state-manager/internal/db/models"
)

func newTestRouter(t *testing.T) (*gin.Engine, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	h := NewHandlers(sqlx.NewDb(db, "postgres"))

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/ui/theme", h.GetTheme())
	r.PUT("/admin/ui-theme", h.PutTheme())
	return r, mock
}

func TestGetTheme_Unset_ReturnsDefault(t *testing.T) {
	r, mock := newTestRouter(t)
	mock.ExpectQuery(`SELECT value FROM system_settings`).
		WillReturnRows(sqlmock.NewRows([]string{"value"})) // no rows -> default

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/ui/theme", nil))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var resp models.UIThemeConfig
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.NotNil(t, resp.ProductName)
	assert.Equal(t, *DefaultTheme().ProductName, *resp.ProductName)
	require.NotNil(t, resp.PrimaryColor)
	assert.Equal(t, *DefaultTheme().PrimaryColor, *resp.PrimaryColor)
}

func TestGetTheme_Configured(t *testing.T) {
	r, mock := newTestRouter(t)
	stored := models.UIThemeConfig{ProductName: strptr("Acme TSM"), PrimaryColor: strptr("#112233")}
	payload, err := json.Marshal(&stored)
	require.NoError(t, err)
	mock.ExpectQuery(`SELECT value FROM system_settings`).
		WillReturnRows(sqlmock.NewRows([]string{"value"}).AddRow(string(payload)))

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/ui/theme", nil))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var resp models.UIThemeConfig
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.NotNil(t, resp.ProductName)
	assert.Equal(t, "Acme TSM", *resp.ProductName)
}

func TestGetTheme_DBError(t *testing.T) {
	r, mock := newTestRouter(t)
	mock.ExpectQuery(`SELECT value FROM system_settings`).
		WillReturnError(fmt.Errorf("db error"))

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/ui/theme", nil))
	assert.Equal(t, http.StatusInternalServerError, w.Code)
}

func TestPutTheme_InvalidJSON(t *testing.T) {
	r, _ := newTestRouter(t)
	req := httptest.NewRequest(http.MethodPut, "/admin/ui-theme", bytes.NewReader([]byte("{bad json")))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestPutTheme_InvalidColor(t *testing.T) {
	r, _ := newTestRouter(t)
	body, _ := json.Marshal(map[string]any{"primary_color": "rgb(1,2,3)"})
	req := httptest.NewRequest(http.MethodPut, "/admin/ui-theme", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
}

func TestPutTheme_InvalidURL(t *testing.T) {
	r, _ := newTestRouter(t)
	body, _ := json.Marshal(map[string]any{"logo_url": "javascript:alert(1)"})
	req := httptest.NewRequest(http.MethodPut, "/admin/ui-theme", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
}

func TestPutTheme_DBError(t *testing.T) {
	r, mock := newTestRouter(t)
	mock.ExpectExec(`INSERT INTO system_settings`).
		WillReturnError(fmt.Errorf("db error"))

	body, _ := json.Marshal(map[string]any{"product_name": "Acme"})
	req := httptest.NewRequest(http.MethodPut, "/admin/ui-theme", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusInternalServerError, w.Code)
}

func TestPutTheme_Success(t *testing.T) {
	r, mock := newTestRouter(t)
	mock.ExpectExec(`INSERT INTO system_settings`).
		WithArgs("ui_theme", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	body, _ := json.Marshal(map[string]any{
		"product_name":  "Acme",
		"primary_color": "#5C4EE5",
		"logo_url":      "https://cdn.example.com/logo.svg",
	})
	req := httptest.NewRequest(http.MethodPut, "/admin/ui-theme", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var resp models.UIThemeConfig
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.NotNil(t, resp.ProductName)
	assert.Equal(t, "Acme", *resp.ProductName)
	assert.False(t, resp.UpdatedAt.IsZero())
}

func TestPutTheme_RelativeURL_Allowed(t *testing.T) {
	r, mock := newTestRouter(t)
	mock.ExpectExec(`INSERT INTO system_settings`).
		WithArgs("ui_theme", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	body, _ := json.Marshal(map[string]any{"logo_url": "/assets/logo.svg"})
	req := httptest.NewRequest(http.MethodPut, "/admin/ui-theme", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
}

func TestValidateTheme(t *testing.T) {
	cases := []struct {
		name    string
		in      models.UIThemeConfig
		wantErr bool
	}{
		{"all empty ok", models.UIThemeConfig{}, false},
		{"short hex ok", models.UIThemeConfig{PrimaryColor: strptr("#abc")}, false},
		{"long hex ok", models.UIThemeConfig{PrimaryColor: strptr("#5C4EE5")}, false},
		{"https logo ok", models.UIThemeConfig{LogoURL: strptr("https://cdn.example.com/x.png")}, false},
		{"relative logo ok", models.UIThemeConfig{LogoURL: strptr("/static/logo.svg")}, false},
		{"bad color", models.UIThemeConfig{PrimaryColor: strptr("red")}, true},
		{"http insecure", models.UIThemeConfig{LogoURL: strptr("http://cdn.example.com/x.png")}, true},
		{"javascript scheme", models.UIThemeConfig{LogoURL: strptr("javascript:alert(1)")}, true},
		{"protocol relative", models.UIThemeConfig{LogoURL: strptr("//cdn.example.com/x.png")}, true},
		{"url with quote", models.UIThemeConfig{LogoURL: strptr(`https://cdn.example.com/x".png`)}, true},
		{"long product name", models.UIThemeConfig{ProductName: strptr(longString(201))}, true},
		{"200 char product ok", models.UIThemeConfig{ProductName: strptr(longString(200))}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateTheme(&tc.in)
			assert.Equal(t, tc.wantErr, err != nil, "err = %v", err)
		})
	}
}

func longString(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = 'a'
	}
	return string(b)
}
