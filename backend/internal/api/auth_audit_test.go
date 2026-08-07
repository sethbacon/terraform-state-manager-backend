package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"

	"github.com/terraform-state-manager/terraform-state-manager/internal/config"
)

// Auth-event audit-trail coverage: docs/observability.md promises logins and
// logouts land in audit_logs. The OIDC/SAML success paths use the identical
// one-line hook and are exercised end-to-end only against a real IdP.

func TestLogout_AuditsAuthenticatedUser(t *testing.T) {
	h, mock := newReconcileEnv(t, nil)

	mock.ExpectQuery("INSERT INTO audit_logs").WithArgs(
		sqlmock.AnyArg(), // id
		"u1",             // user_id
		nil,              // organization_id
		"auth.logout",
		"user",
		"u1",
		nil,              // metadata
		sqlmock.AnyArg(), // ip
		sqlmock.AnyArg(), // created_at
		nil,              // actor_email: filled by the INSERT's own COALESCE subquery
	).WillReturnRows(auditInsertReturn())

	r := gin.New()
	r.POST("/logout", func(c *gin.Context) { c.Set("user_id", "u1") }, h.LogoutPostHandler())
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/logout", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("logout: status = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("logout must write an auth.logout audit entry: %v", err)
	}
}

func TestLogout_AnonymousWritesNoAudit(t *testing.T) {
	h, mock := newReconcileEnv(t, nil)
	// No INSERT expectation queued: an unauthenticated logout (the route is
	// optionalAuth) must not fabricate an unattributed audit entry.
	r := gin.New()
	r.POST("/logout", h.LogoutPostHandler())
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/logout", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("anonymous logout: status = %d, want 200", w.Code)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("anonymous logout hit the database: %v", err)
	}
}

func TestLDAPLogin_FailureAudited(t *testing.T) {
	// A real provider pointed at an unreachable directory: Authenticate fails
	// fast with a dial error, driving the handler's uniform-401 branch.
	h, mock := newReconcileEnv(t, func(cfg *config.Config) {
		cfg.Auth.LDAP.Enabled = true
		cfg.Auth.LDAP.Host = "127.0.0.1"
		cfg.Auth.LDAP.Port = 1
		cfg.Auth.LDAP.BaseDN = "dc=example,dc=com"
		cfg.Auth.LDAP.BindDN = "cn=svc,dc=example,dc=com"
		cfg.Auth.LDAP.UserFilter = "(uid=%s)"
	})

	mock.ExpectQuery("INSERT INTO audit_logs").WithArgs(
		sqlmock.AnyArg(), // id
		nil,              // user_id: unknown on failure
		nil,              // organization_id
		"auth.login_failed",
		"user",
		nil, // resource_id: no user resolved
		[]byte(`{"provider":"ldap","username":"alice"}`),
		sqlmock.AnyArg(), // ip
		sqlmock.AnyArg(), // created_at
		nil,              // actor_email: filled by the INSERT's own COALESCE subquery
	).WillReturnRows(auditInsertReturn())

	r := gin.New()
	r.POST("/login", h.LDAPLoginHandler())
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/login",
		strings.NewReader(`{"username":"alice","password":"wrong"}`))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("bad credentials: status = %d, want 401 (%s)", w.Code, w.Body.String())
	}
	// The uniform error must not distinguish user-vs-password failures.
	if !strings.Contains(w.Body.String(), "invalid credentials") {
		t.Errorf("expected the uniform 401 body, got %s", w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("failed login must write an auth.login_failed audit entry: %v", err)
	}
}
