// Package middleware provides HTTP middleware for the TSM backend,
// including security headers, rate limiting, authentication, RBAC,
// audit logging, metrics, and setup-token validation.
package middleware

import (
	"strconv"

	"github.com/gin-gonic/gin"
)

// SecurityHeadersConfig defines which security headers to set and their values.
type SecurityHeadersConfig struct {
	// HSTSMaxAge controls the max-age directive of the Strict-Transport-Security
	// header. Set to 0 to omit the header.
	HSTSMaxAge int

	// HSTSIncludeSubdomains adds includeSubDomains to the HSTS header.
	HSTSIncludeSubdomains bool

	// HSTSPreload adds the preload directive to the HSTS header.
	HSTSPreload bool

	// FrameOption sets the X-Frame-Options header (e.g. "DENY", "SAMEORIGIN").
	FrameOption string

	// ContentTypeNosniff enables the X-Content-Type-Options: nosniff header.
	ContentTypeNosniff bool

	// XSSProtection sets the X-XSS-Protection header value.
	XSSProtection string

	// ContentSecurityPolicy sets the Content-Security-Policy header.
	ContentSecurityPolicy string

	// ReferrerPolicy sets the Referrer-Policy header.
	ReferrerPolicy string

	// PermissionsPolicy sets the Permissions-Policy header.
	PermissionsPolicy string

	// CrossOriginEmbedderPolicy sets the Cross-Origin-Embedder-Policy header.
	CrossOriginEmbedderPolicy string

	// CrossOriginOpenerPolicy sets the Cross-Origin-Opener-Policy header.
	CrossOriginOpenerPolicy string

	// CrossOriginResourcePolicy sets the Cross-Origin-Resource-Policy header.
	CrossOriginResourcePolicy string
}

// DefaultSecurityHeadersConfig returns a SecurityHeadersConfig suitable for
// HTML-serving endpoints with strict defaults.
func DefaultSecurityHeadersConfig() SecurityHeadersConfig {
	return SecurityHeadersConfig{
		HSTSMaxAge:                31536000, // 1 year
		HSTSIncludeSubdomains:     true,
		HSTSPreload:               true,
		FrameOption:               "DENY",
		ContentTypeNosniff:        true,
		XSSProtection:             "1; mode=block",
		ContentSecurityPolicy:     "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; font-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'self'; form-action 'self'",
		ReferrerPolicy:            "strict-origin-when-cross-origin",
		PermissionsPolicy:         "camera=(), microphone=(), geolocation=(), interest-cohort=()",
		CrossOriginEmbedderPolicy: "require-corp",
		CrossOriginOpenerPolicy:   "same-origin",
		CrossOriginResourcePolicy: "same-origin",
	}
}

// APISecurityHeadersConfig returns a SecurityHeadersConfig tuned for
// JSON API endpoints. CSP is relaxed because no HTML is served.
func APISecurityHeadersConfig() SecurityHeadersConfig {
	return SecurityHeadersConfig{
		HSTSMaxAge:                31536000,
		HSTSIncludeSubdomains:     true,
		HSTSPreload:               false,
		FrameOption:               "DENY",
		ContentTypeNosniff:        true,
		XSSProtection:             "0",
		ContentSecurityPolicy:     "default-src 'none'; frame-ancestors 'none'",
		ReferrerPolicy:            "no-referrer",
		PermissionsPolicy:         "camera=(), microphone=(), geolocation=(), interest-cohort=()",
		CrossOriginEmbedderPolicy: "require-corp",
		CrossOriginOpenerPolicy:   "same-origin",
		CrossOriginResourcePolicy: "same-origin",
	}
}

// SecurityHeadersMiddleware returns a gin.HandlerFunc that sets security
// headers on every response according to the provided configuration.
func SecurityHeadersMiddleware(cfg SecurityHeadersConfig) gin.HandlerFunc {
	return func(c *gin.Context) {
		// HSTS
		if cfg.HSTSMaxAge > 0 {
			value := "max-age=" + itoa(cfg.HSTSMaxAge)
			if cfg.HSTSIncludeSubdomains {
				value += "; includeSubDomains"
			}
			if cfg.HSTSPreload {
				value += "; preload"
			}
			c.Header("Strict-Transport-Security", value)
		}

		// X-Frame-Options
		if cfg.FrameOption != "" {
			c.Header("X-Frame-Options", cfg.FrameOption)
		}

		// X-Content-Type-Options
		if cfg.ContentTypeNosniff {
			c.Header("X-Content-Type-Options", "nosniff")
		}

		// X-XSS-Protection
		if cfg.XSSProtection != "" {
			c.Header("X-XSS-Protection", cfg.XSSProtection)
		}

		// Content-Security-Policy
		if cfg.ContentSecurityPolicy != "" {
			c.Header("Content-Security-Policy", cfg.ContentSecurityPolicy)
		}

		// Referrer-Policy
		if cfg.ReferrerPolicy != "" {
			c.Header("Referrer-Policy", cfg.ReferrerPolicy)
		}

		// Permissions-Policy
		if cfg.PermissionsPolicy != "" {
			c.Header("Permissions-Policy", cfg.PermissionsPolicy)
		}

		// Cross-Origin-Embedder-Policy
		if cfg.CrossOriginEmbedderPolicy != "" {
			c.Header("Cross-Origin-Embedder-Policy", cfg.CrossOriginEmbedderPolicy)
		}

		// Cross-Origin-Opener-Policy
		if cfg.CrossOriginOpenerPolicy != "" {
			c.Header("Cross-Origin-Opener-Policy", cfg.CrossOriginOpenerPolicy)
		}

		// Cross-Origin-Resource-Policy
		if cfg.CrossOriginResourcePolicy != "" {
			c.Header("Cross-Origin-Resource-Policy", cfg.CrossOriginResourcePolicy)
		}

		c.Next()
	}
}

// itoa is a small helper that converts an integer to its decimal string representation.
func itoa(n int) string {
	return strconv.Itoa(n)
}
