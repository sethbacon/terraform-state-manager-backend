package api

import (
	"net"
	"net/http"
	"strings"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"

	identitycrypto "github.com/sethbacon/terraform-suite-identity/identity/crypto"

	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
	"github.com/terraform-state-manager/terraform-state-manager/internal/services/notify"
)

// newSMTPSettingsEnv wires the shared SMTP relay settings endpoints. When smtp
// is nil the handler is left unwired (WithSMTPSettings not called), so those
// endpoints report 503; otherwise smtp is the live, shared config pointer the
// Notifier and handler both observe.
func newSMTPSettingsEnv(t *testing.T, smtp *notify.SMTPConfig) *sourcesEnv {
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
	notifier := notify.New(repositories.NewNotificationChannelRepository(db), smtp, tc, nil)
	h := NewNotificationHandlers(db, nil, notifier, tc)
	if smtp != nil {
		h = h.WithSMTPSettings(repositories.NewSystemSettingsRepository(db), smtp)
	}

	r := gin.New()
	v1 := r.Group("/api/v1/notifications")
	v1.GET("/smtp-config", h.GetSMTPConfig())
	v1.PUT("/smtp-config", h.PutSMTPConfig())
	v1.POST("/test-email", h.TestEmail())
	return &sourcesEnv{r: r, mock: mock}
}

// smtpTestFreePort returns a port that was just bound and released, so a dial to
// it is refused immediately (a fast, deterministic "relay unreachable").
func smtpTestFreePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}

func TestSMTPConfig_Unwired503(t *testing.T) {
	e := newSMTPSettingsEnv(t, nil)
	if w := e.do(http.MethodGet, "/api/v1/notifications/smtp-config", ""); w.Code != http.StatusServiceUnavailable {
		t.Errorf("GET unwired: status = %d, want 503", w.Code)
	}
	if w := e.do(http.MethodPut, "/api/v1/notifications/smtp-config", `{"host":"x"}`); w.Code != http.StatusServiceUnavailable {
		t.Errorf("PUT unwired: status = %d, want 503", w.Code)
	}
}

func TestGetSMTPConfig(t *testing.T) {
	smtp := &notify.SMTPConfig{Host: "smtp.example.com", Port: 587, From: "tsm@example.com", UseTLS: true}
	e := newSMTPSettingsEnv(t, smtp)
	e.mock.ExpectQuery("SELECT notifications_config FROM system_settings").
		WillReturnRows(sqlmock.NewRows([]string{"notifications_config"}).
			AddRow([]byte(`{"smtp":{"host":"smtp.example.com","password_encrypted":"deadbeef"}}`)))
	w := e.do(http.MethodGet, "/api/v1/notifications/smtp-config", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"password_configured":true`) {
		t.Errorf("expected password_configured true, got %s", w.Body.String())
	}
	if strings.Contains(w.Body.String(), "deadbeef") {
		t.Error("GET must never return the encrypted password")
	}
}

func TestPutSMTPConfig_BadInput(t *testing.T) {
	e := newSMTPSettingsEnv(t, &notify.SMTPConfig{})
	cases := map[string]string{
		"bad json": `{bad`,
		"bad port": `{"host":"x","port":70000}`,
		"bad from": `{"host":"x","port":587,"from":"not-an-email"}`,
	}
	for why, body := range cases {
		if w := e.do(http.MethodPut, "/api/v1/notifications/smtp-config", body); w.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", why, w.Code)
		}
	}
}

func TestPutSMTPConfig_SuccessWithPassword(t *testing.T) {
	smtp := &notify.SMTPConfig{}
	e := newSMTPSettingsEnv(t, smtp)
	e.mock.ExpectQuery("SELECT notifications_config FROM system_settings").
		WillReturnRows(sqlmock.NewRows([]string{"notifications_config"}).AddRow(nil))
	e.mock.ExpectExec("UPDATE system_settings SET notifications_configured").
		WillReturnResult(sqlmock.NewResult(0, 1))

	body := `{"host":"smtp.example.com","port":465,"username":"u","password":"secret","from":"tsm@example.com","use_tls":true}`
	w := e.do(http.MethodPut, "/api/v1/notifications/smtp-config", body)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", w.Code, w.Body.String())
	}
	// The live config pointer must be updated in place so the Notifier observes it.
	if smtp.Host != "smtp.example.com" || smtp.Port != 465 || smtp.Password != "secret" || !smtp.UseTLS {
		t.Errorf("live smtp config not updated in place: %+v", *smtp)
	}
	if !strings.Contains(w.Body.String(), `"password_configured":true`) {
		t.Errorf("expected password_configured true, got %s", w.Body.String())
	}
}

func TestPutSMTPConfig_KeepsExistingPassword(t *testing.T) {
	smtp := &notify.SMTPConfig{}
	e := newSMTPSettingsEnv(t, smtp)
	e.mock.ExpectQuery("SELECT notifications_config FROM system_settings").
		WillReturnRows(sqlmock.NewRows([]string{"notifications_config"}).
			AddRow([]byte(`{"smtp":{"password_encrypted":"existing"}}`)))
	e.mock.ExpectExec("UPDATE system_settings SET notifications_configured").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// No password field => the previously stored (encrypted) password is kept.
	body := `{"host":"smtp.example.com","port":587,"from":"tsm@example.com"}`
	w := e.do(http.MethodPut, "/api/v1/notifications/smtp-config", body)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"password_configured":true`) {
		t.Errorf("expected password_configured true (existing kept), got %s", w.Body.String())
	}
}

func TestTestEmail_Validation(t *testing.T) {
	e := newSMTPSettingsEnv(t, &notify.SMTPConfig{}) // no Host => "not configured"
	cases := map[string]string{
		"bad json":         `{bad`,
		"no recipients":    `{"recipients":[]}`,
		"bad recipient":    `{"recipients":["nope"]}`,
		"smtp not set yet": `{"recipients":["ops@example.com"]}`,
	}
	for why, body := range cases {
		if w := e.do(http.MethodPost, "/api/v1/notifications/test-email", body); w.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400 (%s)", why, w.Code, w.Body.String())
		}
	}
}

func TestTestEmail_SendFailureReturns200(t *testing.T) {
	// A configured but unreachable relay: SendTestEmail fails to dial, and the
	// handler still returns 200 with {success:false} (never a 5xx to the UI).
	smtp := &notify.SMTPConfig{Host: "127.0.0.1", Port: smtpTestFreePort(t), From: "tsm@example.com"}
	e := newSMTPSettingsEnv(t, smtp)
	w := e.do(http.MethodPost, "/api/v1/notifications/test-email",
		`{"recipients":["ops@example.com"],"subject":"hi"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"success":false`) {
		t.Errorf("expected success:false on send failure, got %s", w.Body.String())
	}
}
