package mtls

import (
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"

	idplatformadmin "github.com/sethbacon/terraform-suite-identity/identity/platformadmin"

	"github.com/terraform-state-manager/terraform-state-manager/internal/platformadmin"
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
//
// THE SCOPES PUBLISHED HERE ARE CARRIER-RESOLVED, NOT AS CONFIGURED (#476).
//
// A subject→scope mapping is a claim written in a config file. Before this, that
// claim was published verbatim, so `scopes: ["admin"]` made the caller a platform
// administrator with no grant record, no audit entry, and no revocation short of
// editing configuration and restarting — while every other credential class was
// already governed: sessions through platformadmin.Service.SessionScopes, API
// keys stripped by idplatformadmin.KeyScopes. mTLS was the one that kept its own
// answer to "who is a platform administrator?".
//
// A mapping that names a user is now resolved through the carrier, so `admin`
// holds only while that user holds a carrier row and stops holding the moment
// the row is revoked. A mapping that names no user cannot reach `admin` at all:
// NewProvider refuses to build one at startup, and the publish path below strips
// it anyway.
func AuthMiddleware(p *Provider, platformAdmins *platformadmin.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		if p == nil || c.Request.TLS == nil || len(c.Request.TLS.VerifiedChains) == 0 {
			c.Next()
			return
		}

		// The leaf of the first verified chain is the authenticated client cert.
		leaf := c.Request.TLS.VerifiedChains[0][0]
		subject, scopes, userID, err := p.Authenticate(leaf)
		if err != nil {
			slog.Debug("mTLS: verified client cert has no mapping", "cn", leaf.Subject.CommonName, "error", err)
			c.Next()
			return
		}

		effective, status, msg := certificateScopes(c, platformAdmins, userID, scopes)
		if status != 0 {
			// An authority question that did not resolve is not a completed
			// "no". Aborting rather than continuing unelevated matches the JWT
			// path's treatment of the same failure: a silent downgrade takes the
			// admin surface away from an administrator during exactly the
			// incident they need it in, with nothing saying why.
			c.AbortWithStatusJSON(status, gin.H{"error": msg})
			return
		}

		c.Set("auth_method", "mtls")
		c.Set("mtls_subject", subject)
		c.Set("scopes", effective)

		// Published under its OWN key rather than as "user_id" — deliberately.
		// Many sites read "user_id" and some authorize with it, so setting it
		// here would make an mTLS caller indistinguishable from a signed-in user
		// across all of them: a second, wider change than binding a certificate
		// to a carrier row. The binding is recorded so it can be audited and so
		// that decision can be revisited on its own evidence.
		if userID != "" {
			c.Set("mtls_user_id", userID)
		}

		slog.Info("mTLS authenticated", "subject", subject, "user_id", userID, "scopes", effective)
		c.Next()
	}
}

// certificateScopes turns a mapping's CONFIGURED scopes into the effective ones.
//
// Two paths, and neither can publish `admin` on the strength of the config
// alone:
//
//   - the mapping NAMES A USER — the scopes go through
//     platformadmin.Service.CertificateScopes, the STRICT reading, so `admin`
//     survives only while that user holds a carrier row and revoking it disarms
//     the certificate on the next request rather than at the next restart;
//   - the mapping names NO user — KeyScopes, the API-key treatment: `admin`
//     removed, always. NewProvider already refuses to build such a mapping with
//     `admin` in it, so this is the second of two locks on the same door. It is
//     here because the first guards a construction path and this one guards the
//     publish, and only the publish is what the RBAC layer reads.
//
// CertificateScopes, NOT SessionScopes. The session reading is deliberately
// ADDITIVE — it re-adds `admin` the caller already had, to avoid stripping the
// role-template administrators every existing deployment runs on — and putting a
// certificate through it would preserve a config-supplied `admin` rather than
// remove it. The fix would look applied and change nothing.
//
// A nil service degrades to stripping rather than to trusting.
func certificateScopes(c *gin.Context, platformAdmins *platformadmin.Service, userID string, scopes []string) ([]string, int, string) {
	if platformAdmins == nil || userID == "" {
		return idplatformadmin.KeyScopes(scopes), 0, ""
	}
	effective, err := platformAdmins.CertificateScopes(c.Request.Context(), userID, scopes)
	if err != nil {
		return idplatformadmin.KeyScopes(scopes), http.StatusInternalServerError, "Auth check failed"
	}
	return effective, 0, ""
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
