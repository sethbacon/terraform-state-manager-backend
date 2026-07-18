package api

import (
	"encoding/json"
	"net/http"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"

	identitycrypto "github.com/sethbacon/terraform-suite-identity/identity/crypto"

	"github.com/terraform-state-manager/terraform-state-manager/internal/config"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
	"github.com/terraform-state-manager/terraform-state-manager/internal/services/notify"
)

// newAPIKeyExpiryEnv wires the API-key-expiry settings endpoints. When
// notifCfg is nil the handler is left unwired (WithAPIKeyExpirySettings not
// called), so those endpoints report 503; otherwise notifCfg is the live,
// shared config pointer the handler reads/mutates in place.
func newAPIKeyExpiryEnv(t *testing.T, notifCfg *config.NotificationsConfig) *sourcesEnv {
	t.Helper()
	t.Setenv("TSM_ENCRYPTION_KEY", testEncryptionKey)
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	tc, err := identitycrypto.NewTokenCipher([]byte(testEncryptionKey))
	if err != nil {
		t.Fatalf("NewTokenCipher: %v", err)
	}
	notifier := notify.New(repositories.NewNotificationChannelRepository(db), &notify.SMTPConfig{}, tc, nil)
	h := NewNotificationHandlers(db, nil, notifier, tc)
	if notifCfg != nil {
		h = h.WithSMTPSettings(repositories.NewSystemSettingsRepository(db), &notify.SMTPConfig{})
		h = h.WithAPIKeyExpirySettings(notifCfg)
	}

	r := gin.New()
	v1 := r.Group("/api/v1/notifications")
	v1.GET("/api-key-expiry", h.GetAPIKeyExpiryConfig())
	v1.PUT("/api-key-expiry", h.PutAPIKeyExpiryConfig())
	return &sourcesEnv{r: r, mock: mock}
}

func TestAPIKeyExpiryConfig_Unwired503(t *testing.T) {
	e := newAPIKeyExpiryEnv(t, nil)
	if w := e.do(http.MethodGet, "/api/v1/notifications/api-key-expiry", ""); w.Code != http.StatusServiceUnavailable {
		t.Errorf("GET unwired: status = %d, want 503", w.Code)
	}
	if w := e.do(http.MethodPut, "/api/v1/notifications/api-key-expiry", `{"api_key_expiring":true}`); w.Code != http.StatusServiceUnavailable {
		t.Errorf("PUT unwired: status = %d, want 503", w.Code)
	}
}

func TestGetAPIKeyExpiryConfig(t *testing.T) {
	notifCfg := &config.NotificationsConfig{
		Enabled:                        true,
		Events:                         config.NotificationEventsConfig{APIKeyExpiring: true},
		APIKeyExpiryWarningDays:        7,
		APIKeyExpiryCheckIntervalHours: 24,
	}
	e := newAPIKeyExpiryEnv(t, notifCfg)
	w := e.do(http.MethodGet, "/api/v1/notifications/api-key-expiry", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", w.Code, w.Body.String())
	}
	var resp NotificationsAPIKeyExpiryResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	want := NotificationsAPIKeyExpiryResponse{Enabled: true, APIKeyExpiring: true, WarningDays: 7, CheckIntervalHours: 24}
	if resp != want {
		t.Errorf("response = %+v, want %+v", resp, want)
	}
}

func TestPutAPIKeyExpiryConfig_BadInput(t *testing.T) {
	e := newAPIKeyExpiryEnv(t, &config.NotificationsConfig{})
	cases := map[string]string{
		"bad json":                `{bad`,
		"negative warning days":   `{"api_key_expiring":true,"api_key_expiry_warning_days":-1}`,
		"negative check interval": `{"api_key_expiring":true,"api_key_expiry_check_interval_hours":-1}`,
	}
	for why, body := range cases {
		if w := e.do(http.MethodPut, "/api/v1/notifications/api-key-expiry", body); w.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400 (%s)", why, w.Code, w.Body.String())
		}
	}
}

func TestPutAPIKeyExpiryConfig_Success(t *testing.T) {
	notifCfg := &config.NotificationsConfig{}
	e := newAPIKeyExpiryEnv(t, notifCfg)
	e.mock.ExpectQuery("SELECT notifications_config FROM system_settings").
		WillReturnRows(sqlmock.NewRows([]string{"notifications_config"}).AddRow(nil))
	e.mock.ExpectExec("UPDATE system_settings SET notifications_configured").
		WillReturnResult(sqlmock.NewResult(0, 1))

	body := `{"api_key_expiring":true,"api_key_expiry_warning_days":14,"api_key_expiry_check_interval_hours":12}`
	w := e.do(http.MethodPut, "/api/v1/notifications/api-key-expiry", body)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", w.Code, w.Body.String())
	}
	// The live config pointer must be updated in place so the background
	// notifier's next tick observes it.
	if !notifCfg.Events.APIKeyExpiring || notifCfg.APIKeyExpiryWarningDays != 14 || notifCfg.APIKeyExpiryCheckIntervalHours != 12 {
		t.Errorf("live notifCfg not updated in place: %+v", *notifCfg)
	}
}

// TestPutAPIKeyExpiryConfig_PreservesSMTPSection guards against the exact
// clobber bug this feature could otherwise introduce: saving the expiry
// section must not wipe out a previously persisted SMTP section (they share
// the same system_settings.notifications_config JSON blob).
func TestPutAPIKeyExpiryConfig_PreservesSMTPSection(t *testing.T) {
	e := newAPIKeyExpiryEnv(t, &config.NotificationsConfig{})
	e.mock.ExpectQuery("SELECT notifications_config FROM system_settings").
		WillReturnRows(sqlmock.NewRows([]string{"notifications_config"}).
			AddRow([]byte(`{"smtp":{"host":"smtp.example.com","password_encrypted":"existing"}}`)))
	e.mock.ExpectExec("UPDATE system_settings SET notifications_configured").
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	body := `{"api_key_expiring":true,"api_key_expiry_warning_days":7,"api_key_expiry_check_interval_hours":24}`
	w := e.do(http.MethodPut, "/api/v1/notifications/api-key-expiry", body)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", w.Code, w.Body.String())
	}
	if err := e.mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestReloadNotificationsExpiryConfigFromDB directly exercises the startup
// reload path (router.go), independent of the HTTP handlers.
func TestReloadNotificationsExpiryConfigFromDB(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()
	settingsRepo := repositories.NewSystemSettingsRepository(db)

	t.Run("nil expiry section leaves defaults untouched", func(t *testing.T) {
		mock.ExpectQuery("SELECT notifications_config FROM system_settings").
			WillReturnRows(sqlmock.NewRows([]string{"notifications_config"}).
				AddRow([]byte(`{"smtp":{"host":"smtp.example.com"}}`)))
		notifCfg := &config.NotificationsConfig{APIKeyExpiryWarningDays: 7, APIKeyExpiryCheckIntervalHours: 24}
		reloadNotificationsExpiryConfigFromDB(notifCfg, settingsRepo)
		if notifCfg.Events.APIKeyExpiring || notifCfg.APIKeyExpiryWarningDays != 7 || notifCfg.APIKeyExpiryCheckIntervalHours != 24 {
			t.Errorf("expected untouched defaults, got %+v", *notifCfg)
		}
	})

	t.Run("persisted expiry section is applied", func(t *testing.T) {
		mock.ExpectQuery("SELECT notifications_config FROM system_settings").
			WillReturnRows(sqlmock.NewRows([]string{"notifications_config"}).
				AddRow([]byte(`{"expiry":{"api_key_expiring":true,"warning_days":3,"check_interval_hours":6}}`)))
		notifCfg := &config.NotificationsConfig{APIKeyExpiryWarningDays: 7, APIKeyExpiryCheckIntervalHours: 24}
		reloadNotificationsExpiryConfigFromDB(notifCfg, settingsRepo)
		if !notifCfg.Events.APIKeyExpiring || notifCfg.APIKeyExpiryWarningDays != 3 || notifCfg.APIKeyExpiryCheckIntervalHours != 6 {
			t.Errorf("expected persisted values applied, got %+v", *notifCfg)
		}
	})

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}
