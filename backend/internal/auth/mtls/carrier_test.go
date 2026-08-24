package mtls

import (
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"

	"github.com/terraform-state-manager/terraform-state-manager/internal/config"
	"github.com/terraform-state-manager/terraform-state-manager/internal/platformadmin"
)

// mTLS was the last credential class outside the platform-admin carrier here
// (#476), as it was in the sibling registry (#876). Sessions go through
// Service.SessionScopes and API keys are stripped by KeyScopes, but a subject
// mapping published whatever the config file said — so `scopes: ["admin"]`
// produced a platform administrator with no grant record, no audit entry, and no
// revocation short of editing configuration and restarting.
//
// The fix could NOT be the registry's. This repo's SessionScopes is deliberately
// additive: it re-adds `admin` the caller already had, so role-template
// administrators are not stripped on upgrade. Passing a certificate through it
// would have PRESERVED the config-supplied `admin` — the fix would look applied
// and change nothing. CertificateScopes is the strict reading, and
// TestSessionReadingWouldNotHaveFixedThis pins exactly that difference.

const (
	testUserID  = "6ba7b810-9dad-11d1-80b4-00c04fd430c8"
	otherUserID = "6ba7b811-9dad-11d1-80b4-00c04fd430c8"
)

func newCarrier(t *testing.T) (*platformadmin.Service, sqlmock.Sqlmock) {
	t.Helper()
	appDB, appMock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New (app): %v", err)
	}
	t.Cleanup(func() { _ = appDB.Close() })
	identityDB, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New (identity): %v", err)
	}
	t.Cleanup(func() { _ = identityDB.Close() })

	svc, err := platformadmin.New(appDB, identityDB)
	if err != nil {
		t.Fatalf("platformadmin.New: %v", err)
	}
	return svc, appMock
}

func expectCarrier(mock sqlmock.Sqlmock, userID string, isAdmin bool) {
	mock.ExpectQuery(`FROM "platform_admins" WHERE user_id`).WithArgs(userID).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(isAdmin))
}

func certFor(cn string) *x509.Certificate {
	return &x509.Certificate{Subject: pkix.Name{CommonName: cn}}
}

func runRequest(t *testing.T, p *Provider, svc *platformadmin.Service, cn string) (int, []string) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	var published []string
	r := gin.New()
	r.Use(AuthMiddleware(p, svc))
	r.GET("/x", func(c *gin.Context) {
		if v, ok := c.Get("scopes"); ok {
			published, _ = v.([]string)
		}
		c.Status(http.StatusOK)
	})

	req := httptest.NewRequest("GET", "/x", nil)
	req.TLS = &tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{certFor(cn)}}}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w.Code, published
}

func hasScope(scopes []string, want string) bool {
	for _, s := range scopes {
		if s == want {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Startup refusal
// ---------------------------------------------------------------------------

func TestNewProviderRefusesAdminWithoutAUser(t *testing.T) {
	_, err := NewProvider(config.MTLSConfig{
		Enabled: true, ClientCAFile: "/ca.crt",
		Mappings: []config.MTLSSubjectMapping{{Subject: "CN=ci", Scopes: []string{"states:read", "admin"}}},
	})
	if err == nil {
		t.Fatal("a mapping carrying `admin` with no user_id was accepted; the carrier is keyed on " +
			"a user, so such a grant can never be audited or revoked")
	}
	if !strings.Contains(err.Error(), "user_id") {
		t.Errorf("the error should tell the operator what to add, got: %v", err)
	}
}

func TestNewProviderAcceptsAdminWithAUser(t *testing.T) {
	p, err := NewProvider(config.MTLSConfig{
		Enabled: true, ClientCAFile: "/ca.crt",
		Mappings: []config.MTLSSubjectMapping{{Subject: "CN=ci", Scopes: []string{"admin"}, UserID: testUserID}},
	})
	if err != nil {
		t.Fatalf("a mapping naming a user should build: %v", err)
	}
	_, _, userID, aerr := p.Authenticate(certFor("ci"))
	if aerr != nil || userID != testUserID {
		t.Errorf("user_id = %q (err %v), want %q — the binding must survive to the middleware", userID, aerr, testUserID)
	}
}

func TestNewProviderRefusesNonUUIDUser(t *testing.T) {
	_, err := NewProvider(config.MTLSConfig{
		Enabled: true, ClientCAFile: "/ca.crt",
		Mappings: []config.MTLSSubjectMapping{{Subject: "CN=ci", Scopes: []string{"states:read"}, UserID: "alice@example.com"}},
	})
	if err == nil {
		t.Fatal("a non-UUID user_id was accepted; it would never match a carrier row and would " +
			"fail silently as 'not an administrator'")
	}
}

func TestNewProviderRefusesDuplicateSubjects(t *testing.T) {
	_, err := NewProvider(config.MTLSConfig{
		Enabled: true, ClientCAFile: "/ca.crt",
		Mappings: []config.MTLSSubjectMapping{
			{Subject: "CN=ci", Scopes: []string{"states:read"}, UserID: testUserID},
			{Subject: "cn=CI", Scopes: []string{"states:write"}, UserID: otherUserID},
		},
	})
	if err == nil {
		t.Fatal("a repeated subject was accepted; last-write-wins would bind the certificate to a " +
			"different user than the line the operator is reading")
	}
}

// ---------------------------------------------------------------------------
// Runtime: the carrier decides, not the config file
// ---------------------------------------------------------------------------

func TestAdminHoldsOnlyWhileTheCarrierRowDoes(t *testing.T) {
	for _, tc := range []struct {
		name      string
		granted   bool
		wantAdmin bool
	}{
		{"carrier row present — admin holds", true, true},
		{"carrier row absent — admin does not hold", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, err := NewProvider(config.MTLSConfig{
				Enabled: true, ClientCAFile: "/ca.crt",
				Mappings: []config.MTLSSubjectMapping{
					{Subject: "CN=ci", Scopes: []string{"states:read", "admin"}, UserID: testUserID},
				},
			})
			if err != nil {
				t.Fatalf("NewProvider: %v", err)
			}
			svc, mock := newCarrier(t)
			expectCarrier(mock, testUserID, tc.granted)

			status, scopes := runRequest(t, p, svc, "ci")
			if status != http.StatusOK {
				t.Fatalf("status = %d, want 200", status)
			}
			if got := hasScope(scopes, "admin"); got != tc.wantAdmin {
				t.Errorf("admin in published scopes = %v, want %v (scopes=%v)", got, tc.wantAdmin, scopes)
			}
			if !hasScope(scopes, "states:read") {
				t.Errorf("states:read was lost; only `admin` is the carrier's business (scopes=%v)", scopes)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("the carrier was not consulted: %v", err)
			}
		})
	}
}

// THE TEST THAT PINS WHY THIS REPO NEEDED A DIFFERENT FIX.
//
// Service.SessionScopes is additive by design: given scopes that already carry
// `admin`, it re-adds it even when the carrier says no. Routing a certificate
// through it — the obvious port of the registry's fix — would therefore have
// left a config-supplied `admin` in place. This asserts the two readings differ
// on exactly that input, so a future refactor that "simplifies" one into the
// other fails here rather than silently reopening #476.
func TestSessionReadingWouldNotHaveFixedThis(t *testing.T) {
	ctx := t.Context()
	claimed := []string{"states:read", "admin"}

	svc, mock := newCarrier(t)
	expectCarrier(mock, testUserID, false)
	session, err := svc.SessionScopes(ctx, testUserID, claimed)
	if err != nil {
		t.Fatalf("SessionScopes: %v", err)
	}
	if !hasScope(session, "admin") {
		t.Fatal("SessionScopes is no longer additive — if that is intended, this test and the " +
			"reason CertificateScopes exists both need revisiting")
	}

	svc2, mock2 := newCarrier(t)
	expectCarrier(mock2, testUserID, false)
	cert, err := svc2.CertificateScopes(ctx, testUserID, claimed)
	if err != nil {
		t.Fatalf("CertificateScopes: %v", err)
	}
	if hasScope(cert, "admin") {
		t.Error("CertificateScopes kept `admin` for a user with no carrier row; it must be the " +
			"STRICT reading, or a config file grants platform administration again")
	}
}

func TestCarrierLookupFailureIsARefusalToAnswer(t *testing.T) {
	p, err := NewProvider(config.MTLSConfig{
		Enabled: true, ClientCAFile: "/ca.crt",
		Mappings: []config.MTLSSubjectMapping{{Subject: "CN=ci", Scopes: []string{"admin"}, UserID: testUserID}},
	})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	svc, mock := newCarrier(t)
	mock.ExpectQuery(`FROM "platform_admins" WHERE user_id`).WithArgs(testUserID).
		WillReturnError(errBoom{})

	status, _ := runRequest(t, p, svc, "ci")
	if status != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 — an authority question that did not resolve must not be "+
			"served as a completed 'no'", status)
	}
}

type errBoom struct{}

func (errBoom) Error() string { return "carrier unavailable" }

// A mapping with no user cannot publish `admin` even if one reaches the publish
// path. NewProvider refuses to build such a mapping, so this asserts the second
// lock.
func TestPublishStripsAdminWhenNoUserIsNamed(t *testing.T) {
	p := &Provider{mappings: map[string]subjectMapping{
		"cn=ci": {scopes: []string{"states:read", "admin"}},
	}}
	svc, _ := newCarrier(t)

	status, scopes := runRequest(t, p, svc, "ci")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if hasScope(scopes, "admin") {
		t.Errorf("admin published for a mapping naming no user (scopes=%v)", scopes)
	}
	if !hasScope(scopes, "states:read") {
		t.Errorf("states:read was lost (scopes=%v)", scopes)
	}
}

// A nil carrier is a deployment that never built one — the unit-test rig, among
// others. It must degrade to stripping, not to trusting the config file.
func TestNilCarrierStripsRatherThanTrusts(t *testing.T) {
	p := &Provider{mappings: map[string]subjectMapping{
		"cn=ci": {scopes: []string{"states:read", "admin"}, userID: testUserID},
	}}

	status, scopes := runRequest(t, p, nil, "ci")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if hasScope(scopes, "admin") {
		t.Errorf("admin published with no carrier to confirm it (scopes=%v)", scopes)
	}
}

// An ordinary machine credential needs no user and is not charged a lookup.
func TestNonAdminMappingNeedsNoUserAndNoLookup(t *testing.T) {
	p, err := NewProvider(config.MTLSConfig{
		Enabled: true, ClientCAFile: "/ca.crt",
		Mappings: []config.MTLSSubjectMapping{{Subject: "CN=ci", Scopes: []string{"states:read", "states:write"}}},
	})
	if err != nil {
		t.Fatalf("an ordinary mapping should need no user_id: %v", err)
	}
	svc, mock := newCarrier(t)

	status, scopes := runRequest(t, p, svc, "ci")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if len(scopes) != 2 {
		t.Errorf("scopes = %v, want the two configured ones unchanged", scopes)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unexpected carrier traffic for a mapping with no user: %v", err)
	}
}
