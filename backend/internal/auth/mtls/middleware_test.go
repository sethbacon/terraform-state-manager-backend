package mtls

import (
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/terraform-state-manager/terraform-state-manager/internal/config"
)

func init() { gin.SetMode(gin.TestMode) }

func middlewareProvider(t *testing.T) *Provider {
	t.Helper()
	p, err := NewProvider(config.MTLSConfig{
		Enabled:      true,
		ClientCAFile: "/etc/tsm/ca.pem",
		Mappings:     []config.MTLSSubjectMapping{{Subject: "CN=SVC-Drift", Scopes: []string{"state:drift"}}},
	})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	return p
}

// serveMTLS runs a request through AuthMiddleware with the given TLS state and
// reports the auth context it established.
func serveMTLS(p *Provider, state *tls.ConnectionState) (authMethod string, scopes []string, code int) {
	r := gin.New()
	r.Use(AuthMiddleware(p))
	r.GET("/", func(c *gin.Context) {
		authMethod = c.GetString("auth_method")
		if v, ok := c.Get("scopes"); ok {
			scopes, _ = v.([]string)
		}
		c.Status(http.StatusOK)
	})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.TLS = state
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return authMethod, scopes, w.Code
}

func TestAuthMiddleware_VerifiedCertGrantsScopes(t *testing.T) {
	leaf := &x509.Certificate{Subject: pkix.Name{CommonName: "SVC-Drift"}}
	method, scopes, code := serveMTLS(middlewareProvider(t), &tls.ConnectionState{
		VerifiedChains: [][]*x509.Certificate{{leaf}},
	})
	if code != http.StatusOK || method != "mtls" {
		t.Fatalf("auth_method = %q (code %d), want mtls", method, code)
	}
	if len(scopes) != 1 || scopes[0] != "state:drift" {
		t.Errorf("scopes = %v", scopes)
	}
}

func TestAuthMiddleware_PassThroughCases(t *testing.T) {
	p := middlewareProvider(t)

	// No TLS at all (plain HTTP) → untouched.
	if method, _, code := serveMTLS(p, nil); method != "" || code != http.StatusOK {
		t.Errorf("plain http: method=%q code=%d", method, code)
	}

	// TLS without verified chains (cert presented but NOT CA-verified) must be
	// ignored — trusting PeerCertificates here would be an auth bypass.
	unverified := &tls.ConnectionState{
		PeerCertificates: []*x509.Certificate{{Subject: pkix.Name{CommonName: "SVC-Drift"}}},
	}
	if method, scopes, _ := serveMTLS(p, unverified); method != "" || scopes != nil {
		t.Errorf("unverified cert must not authenticate: method=%q scopes=%v", method, scopes)
	}

	// Verified but unmapped subject → passes through unauthenticated.
	stranger := &tls.ConnectionState{
		VerifiedChains: [][]*x509.Certificate{{{Subject: pkix.Name{CommonName: "SVC-Unknown"}}}},
	}
	if method, _, code := serveMTLS(p, stranger); method != "" || code != http.StatusOK {
		t.Errorf("unmapped cert: method=%q code=%d", method, code)
	}

	// Nil provider (mTLS disabled) → no-op.
	if method, _, _ := serveMTLS(nil, &tls.ConnectionState{}); method != "" {
		t.Errorf("nil provider must no-op, got method=%q", method)
	}
}

func TestRequireMTLS(t *testing.T) {
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Next() }) // no mtls_subject set
	r.GET("/machine", RequireMTLS(), func(c *gin.Context) { c.Status(http.StatusOK) })

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/machine", nil))
	if w.Code != http.StatusUnauthorized {
		t.Errorf("without mTLS: status = %d, want 401", w.Code)
	}

	r2 := gin.New()
	r2.Use(func(c *gin.Context) { c.Set("mtls_subject", "CN=SVC-Drift"); c.Next() })
	r2.GET("/machine", RequireMTLS(), func(c *gin.Context) { c.Status(http.StatusOK) })
	w = httptest.NewRecorder()
	r2.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/machine", nil))
	if w.Code != http.StatusOK {
		t.Errorf("with mTLS subject: status = %d, want 200", w.Code)
	}
}
