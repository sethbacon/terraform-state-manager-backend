package mtls

import (
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"
)

// AuthMiddleware authenticates requests by a VERIFIED mTLS client certificate.
// It is additive: a request without a (mapped) verified cert passes through
// untouched so other auth methods (JWT cookie) still apply. When a verified cert
// maps to scopes, it sets auth_method=mtls + scopes so RequireScope and the JWT
// AuthMiddleware treat the request as authenticated.
//
// SECURITY: it inspects only tls.ConnectionState.VerifiedChains — populated by
// the handshake exclusively for certificates verified against the configured
// client CA pool. It deliberately does NOT read PeerCertificates, which may hold
// a presented-but-unverified certificate (an auth-bypass if trusted). If the TLS
// server is not configured to verify client certs, VerifiedChains is empty and
// this middleware is a safe no-op.
func AuthMiddleware(p *Provider) gin.HandlerFunc {
	return func(c *gin.Context) {
		if p == nil || c.Request.TLS == nil || len(c.Request.TLS.VerifiedChains) == 0 {
			c.Next()
			return
		}

		// The leaf of the first verified chain is the authenticated client cert.
		leaf := c.Request.TLS.VerifiedChains[0][0]
		subject, scopes, err := p.Authenticate(leaf)
		if err != nil {
			slog.Debug("mTLS: verified client cert has no mapping", "cn", leaf.Subject.CommonName, "error", err)
			c.Next()
			return
		}

		c.Set("auth_method", "mtls")
		c.Set("mtls_subject", subject)
		c.Set("scopes", scopes)
		slog.Info("mTLS authenticated", "subject", subject, "scopes", scopes)
		c.Next()
	}
}

// RequireMTLS rejects requests that were not authenticated by a verified mTLS
// client certificate. Use on machine-to-machine-only routes.
func RequireMTLS() gin.HandlerFunc {
	return func(c *gin.Context) {
		if _, ok := c.Get("mtls_subject"); !ok {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "mTLS client certificate required"})
			return
		}
		c.Next()
	}
}
