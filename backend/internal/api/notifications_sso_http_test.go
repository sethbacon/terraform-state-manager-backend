package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"

	"github.com/terraform-state-manager/terraform-state-manager/internal/config"
	"github.com/terraform-state-manager/terraform-state-manager/internal/crypto"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
	"github.com/terraform-state-manager/terraform-state-manager/internal/services/notify"
)

// ---------------------------------------------------------------------------
// Notification channel handlers
// ---------------------------------------------------------------------------

var notifChannelCols = []string{"id", "name", "type", "encrypted_target", "events", "enabled",
	"last_status", "last_error", "last_sent_at", "created_at", "updated_at"}

func notifChannelRow(t *testing.T, target string) *sqlmock.Rows {
	t.Helper()
	enc, err := crypto.Encrypt([]byte(target))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	return sqlmock.NewRows(notifChannelCols).
		AddRow("n1", "ops", "webhook", enc, []byte(`{drift_detected}`), true, nil, nil, nil, "2026-06-10", "2026-06-10")
}

func newNotificationsEnv(t *testing.T) *sourcesEnv {
	t.Helper()
	t.Setenv("TSM_ENCRYPTION_KEY", "0123456789abcdef0123456789abcdef")
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	notifier := notify.New(repositories.NewNotificationChannelRepository(db), &notify.SMTPConfig{})
	h := NewNotificationHandlers(db, nil, notifier)

	r := gin.New()
	v1 := r.Group("/api/v1/notifications")
	v1.GET("/channels", h.ListChannels())
	v1.POST("/channels", h.CreateChannel())
	v1.PUT("/channels/:id", h.UpdateChannel())
	v1.DELETE("/channels/:id", h.DeleteChannel())
	v1.POST("/channels/:id/test", h.TestChannel())
	return &sourcesEnv{r: r, mock: mock}
}

func TestNotificationChannels_CRUD(t *testing.T) {
	e := newNotificationsEnv(t)

	e.mock.ExpectQuery("SELECT .+ FROM notification_channels ORDER BY").
		WillReturnRows(notifChannelRow(t, "https://hooks.example.com/x"))
	w := e.do(http.MethodGet, "/api/v1/notifications/channels", "")
	if w.Code != http.StatusOK {
		t.Fatalf("list: status = %d", w.Code)
	}
	if strings.Contains(w.Body.String(), "hooks.example.com") {
		t.Error("list leaked a decrypted/encoded target URL")
	}

	// Validation: type, events, target URL shape, target required on create.
	for body, why := range map[string]string{
		`{"name":"x"}`:                                              "missing type",
		`{"name":"x","type":"pager"}`:                               "unknown type",
		`{"name":"x","type":"webhook","events":["nope"]}`:           "unknown event",
		`{"name":"x","type":"webhook","target":"ftp://bad"}`:        "non-http target",
		`{"name":"x","type":"webhook"}`:                             "missing target",
		`{"name":"x","type":"slack","target":"https://h.example"}@`: "", // sentinel, skipped below
	} {
		if why == "" {
			continue
		}
		if w := e.do(http.MethodPost, "/api/v1/notifications/channels", body); w.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", why, w.Code)
		}
	}

	e.mock.ExpectQuery("INSERT INTO notification_channels").
		WillReturnRows(notifChannelRow(t, "https://hooks.example.com/x"))
	w = e.do(http.MethodPost, "/api/v1/notifications/channels",
		`{"name":"ops","type":"webhook","target":"https://hooks.example.com/x","events":["drift_detected"]}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: status = %d (%s)", w.Code, w.Body.String())
	}

	// Update with a blank target keeps the existing secret (enc arg nil).
	e.mock.ExpectQuery("UPDATE notification_channels").
		WillReturnRows(notifChannelRow(t, "https://hooks.example.com/x"))
	w = e.do(http.MethodPut, "/api/v1/notifications/channels/n1",
		`{"name":"ops","type":"webhook","events":["drift_detected"],"enabled":false}`)
	if w.Code != http.StatusOK {
		t.Fatalf("update: status = %d (%s)", w.Code, w.Body.String())
	}

	// Updating a deleted channel → 404 (repo returns nil on no rows).
	e.mock.ExpectQuery("UPDATE notification_channels").
		WillReturnRows(sqlmock.NewRows(notifChannelCols))
	if w := e.do(http.MethodPut, "/api/v1/notifications/channels/ghost",
		`{"name":"x","type":"slack"}`); w.Code != http.StatusNotFound {
		t.Errorf("update missing: status = %d, want 404", w.Code)
	}

	e.mock.ExpectExec("DELETE FROM notification_channels").WithArgs("n1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	if w := e.do(http.MethodDelete, "/api/v1/notifications/channels/n1", ""); w.Code != http.StatusNoContent {
		t.Errorf("delete: status = %d, want 204", w.Code)
	}
}

func TestNotificationChannels_TestEndpoint(t *testing.T) {
	e := newNotificationsEnv(t)

	received := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		received = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	e.mock.ExpectQuery("FROM notification_channels WHERE id").WithArgs("n1").
		WillReturnRows(notifChannelRow(t, srv.URL))
	e.mock.ExpectExec("UPDATE notification_channels").WillReturnResult(sqlmock.NewResult(0, 1))
	w := e.do(http.MethodPost, "/api/v1/notifications/channels/n1/test", "")
	if w.Code != http.StatusOK || !received {
		t.Fatalf("test: status = %d, received = %v (%s)", w.Code, received, w.Body.String())
	}

	// Missing channel surfaces the notifier error.
	e.mock.ExpectQuery("FROM notification_channels WHERE id").WithArgs("ghost").
		WillReturnRows(sqlmock.NewRows(notifChannelCols))
	if w := e.do(http.MethodPost, "/api/v1/notifications/channels/ghost/test", ""); w.Code != http.StatusBadGateway {
		t.Errorf("missing channel: status = %d, want 502", w.Code)
	}
}

// ---------------------------------------------------------------------------
// SSO / OIDC admin views (AuthHandlers with all providers disabled)
// ---------------------------------------------------------------------------

func newSSOEnv(t *testing.T, mutate func(*config.Config)) *sourcesEnv {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	cfg := &config.Config{}
	cfg.Auth.OIDC.IssuerURL = "https://idp.example.com"
	cfg.Auth.OIDC.ClientID = "tsm"
	cfg.Auth.OIDC.GroupClaimName = "groups"
	cfg.Auth.OIDC.DefaultRole = "viewer"
	if mutate != nil {
		mutate(cfg)
	}

	h, err := NewAuthHandlers(cfg, db)
	if err != nil {
		t.Fatalf("NewAuthHandlers: %v", err)
	}

	r := gin.New()
	admin := r.Group("/api/v1/admin")
	admin.GET("/sso", h.SSOConfigHandler())
	admin.GET("/oidc/config", h.OIDCConfigHandler())
	admin.PUT("/oidc/group-mapping", h.UpdateOIDCGroupMapping())
	admin.GET("/identity-group-mappings", h.IdentityGroupMappingsHandler())
	admin.GET("/mtls", h.MTLSConfigHandler())
	return &sourcesEnv{r: r, mock: mock}
}

func TestSSOConfigHandler_OmitsSecrets(t *testing.T) {
	e := newSSOEnv(t, func(cfg *config.Config) {
		cfg.Auth.OIDC.ClientSecret = "super-secret-client"
		cfg.Auth.LDAP.Enabled = true
		cfg.Auth.LDAP.Host = "ldap.example.com"
		cfg.Auth.LDAP.BaseDN = "dc=example,dc=com"
		cfg.Auth.LDAP.BindDN = "cn=svc,dc=example,dc=com"
		cfg.Auth.LDAP.UserFilter = "(uid=%s)"
		cfg.Auth.LDAP.BindPassword = "super-secret-bind"
		cfg.Auth.MTLS.Enabled = true
		cfg.Auth.MTLS.ClientCAFile = "/etc/tsm/ca.pem"
		cfg.Auth.SCIM.Enabled = true
	})

	w := e.do(http.MethodGet, "/api/v1/admin/sso", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	body := w.Body.String()
	for _, leaked := range []string{"super-secret-client", "super-secret-bind"} {
		if strings.Contains(body, leaked) {
			t.Errorf("SSO view leaked a secret: %s", leaked)
		}
	}
	for _, want := range []string{"idp.example.com", "ldap.example.com", "/etc/tsm/ca.pem", `"scim"`} {
		if !strings.Contains(body, want) {
			t.Errorf("SSO view missing %s", want)
		}
	}
}

func TestOIDCConfig_OverlayPrecedence(t *testing.T) {
	e := newSSOEnv(t, nil)

	// No overlay row → file config served.
	e.mock.ExpectQuery("SELECT oidc_group_claim_name").WillReturnRows(
		sqlmock.NewRows([]string{"oidc_group_claim_name", "oidc_default_role", "oidc_group_mappings", "updated_at"}))
	w := e.do(http.MethodGet, "/api/v1/admin/oidc/config", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"viewer"`) {
		t.Fatalf("file config: status = %d (%s)", w.Code, w.Body.String())
	}

	// Overlay present → authoritative (claim name, mappings, default role).
	e.mock.ExpectQuery("SELECT oidc_group_claim_name").WillReturnRows(
		sqlmock.NewRows([]string{"oidc_group_claim_name", "oidc_default_role", "oidc_group_mappings", "updated_at"}).
			AddRow("memberOf", "operator", []byte(`[{"group":"platform","organization":"default","role":"editor"}]`), "2026-06-10"))
	w = e.do(http.MethodGet, "/api/v1/admin/oidc/config", "")
	if w.Code != http.StatusOK {
		t.Fatalf("overlay: status = %d", w.Code)
	}
	for _, want := range []string{`"memberOf"`, `"operator"`, `"platform"`} {
		if !strings.Contains(w.Body.String(), want) {
			t.Errorf("overlay not authoritative, missing %s: %s", want, w.Body.String())
		}
	}
}

func TestUpdateOIDCGroupMapping(t *testing.T) {
	e := newSSOEnv(t, nil)

	if w := e.do(http.MethodPut, "/api/v1/admin/oidc/group-mapping",
		`{"group_mappings":[{"group":"x","organization":"","role":"viewer"}]}`); w.Code != http.StatusBadRequest {
		t.Errorf("incomplete mapping: status = %d, want 400", w.Code)
	}

	e.mock.ExpectExec("INSERT INTO sso_settings").
		WillReturnResult(sqlmock.NewResult(0, 1))
	// The response re-reads the effective config.
	e.mock.ExpectQuery("SELECT oidc_group_claim_name").WillReturnRows(
		sqlmock.NewRows([]string{"oidc_group_claim_name", "oidc_default_role", "oidc_group_mappings", "updated_at"}).
			AddRow("groups", "viewer", []byte(`[{"group":"platform","organization":"default","role":"editor"}]`), "2026-06-10"))
	w := e.do(http.MethodPut, "/api/v1/admin/oidc/group-mapping",
		`{"group_claim_name":"groups","default_role":"viewer","group_mappings":[{"group":" platform ","organization":"default","role":"editor"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("update: status = %d (%s)", w.Code, w.Body.String())
	}
	if err := e.mock.ExpectationsWereMet(); err != nil {
		t.Errorf("overlay upsert missing: %v", err)
	}
}

func TestIdentityGroupMappings_OnlyEnabledProviders(t *testing.T) {
	e := newSSOEnv(t, nil) // SAML + LDAP disabled
	w := e.do(http.MethodGet, "/api/v1/admin/identity-group-mappings", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if strings.Contains(w.Body.String(), "saml") || strings.Contains(w.Body.String(), "ldap") {
		t.Errorf("disabled providers must be omitted: %s", w.Body.String())
	}

	e2 := newSSOEnv(t, func(cfg *config.Config) {
		cfg.Auth.LDAP.Enabled = true
		cfg.Auth.LDAP.Host = "ldap.example.com"
		cfg.Auth.LDAP.BaseDN = "dc=example,dc=com"
		cfg.Auth.LDAP.BindDN = "cn=svc,dc=example,dc=com"
		cfg.Auth.LDAP.UserFilter = "(uid=%s)"
		cfg.Auth.LDAP.BindPassword = "pw"
		cfg.Auth.LDAP.DefaultRole = "viewer"
		cfg.Auth.LDAP.GroupMappings = []config.LDAPGroupMapping{
			{GroupDN: "cn=ops,dc=example", Organization: "default", Role: "operator"},
		}
	})
	w = e2.do(http.MethodGet, "/api/v1/admin/identity-group-mappings", "")
	if !strings.Contains(w.Body.String(), "cn=ops,dc=example") {
		t.Errorf("enabled LDAP mappings missing: %s", w.Body.String())
	}
}

func TestMTLSConfigHandlerView(t *testing.T) {
	e := newSSOEnv(t, func(cfg *config.Config) {
		cfg.Auth.MTLS.Enabled = true
		cfg.Auth.MTLS.ClientCAFile = "/etc/tsm/ca.pem"
		cfg.Auth.MTLS.Mappings = []config.MTLSSubjectMapping{{Subject: "CN=ci", Scopes: []string{"state:read"}}}
	})
	w := e.do(http.MethodGet, "/api/v1/admin/mtls", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "CN=ci") {
		t.Fatalf("status = %d (%s)", w.Code, w.Body.String())
	}
}
