package middleware

import (
	"strconv"

	"github.com/gin-gonic/gin"
)

// SecurityHeadersConfig holds configuration for security headers.
type SecurityHeadersConfig struct {
	EnableHSTS            bool
	HSTSMaxAge            int
	HSTSIncludeSubdomains bool
	HSTSPreload           bool
	EnableFrameOptions       bool
	FrameOptionsValue        string
	EnableContentTypeOptions bool
	EnableXSSProtection      bool
	ContentSecurityPolicy    string
	ReferrerPolicy           string
	PermissionsPolicy        string
}

// APISecurityHeadersConfig returns security headers suitable for API endpoints.
func APISecurityHeadersConfig() SecurityHeadersConfig {
	return SecurityHeadersConfig{
		EnableHSTS:               true,
		HSTSMaxAge:               31536000,
		HSTSIncludeSubdomains:    true,
		HSTSPreload:              false,
		EnableFrameOptions:       true,
		FrameOptionsValue:        "DENY",
		EnableContentTypeOptions: true,
		EnableXSSProtection:      false,
		ContentSecurityPolicy:    "default-src 'none'; frame-ancestors 'none'",
		ReferrerPolicy:           "no-referrer",
		PermissionsPolicy:        "",
	}
}

// SecurityHeadersMiddleware adds security headers to all responses.
func SecurityHeadersMiddleware(config SecurityHeadersConfig) gin.HandlerFunc {
	return func(c *gin.Context) {
		isTLS := c.Request.TLS != nil || c.GetHeader("X-Forwarded-Proto") == "https"
		if config.EnableHSTS && isTLS {
			hstsValue := "max-age=" + strconv.Itoa(config.HSTSMaxAge)
			if config.HSTSIncludeSubdomains {
				hstsValue += "; includeSubDomains"
			}
			if config.HSTSPreload {
				hstsValue += "; preload"
			}
			c.Header("Strict-Transport-Security", hstsValue)
		}

		if config.EnableFrameOptions && config.FrameOptionsValue != "" {
			c.Header("X-Frame-Options", config.FrameOptionsValue)
		}

		if config.EnableContentTypeOptions {
			c.Header("X-Content-Type-Options", "nosniff")
		}

		if config.EnableXSSProtection {
			c.Header("X-XSS-Protection", "1; mode=block")
		}

		if config.ContentSecurityPolicy != "" {
			c.Header("Content-Security-Policy", config.ContentSecurityPolicy)
		}

		if config.ReferrerPolicy != "" {
			c.Header("Referrer-Policy", config.ReferrerPolicy)
		}

		if config.PermissionsPolicy != "" {
			c.Header("Permissions-Policy", config.PermissionsPolicy)
		}

		c.Header("X-Permitted-Cross-Domain-Policies", "none")
		c.Header("Cross-Origin-Embedder-Policy", "require-corp")
		c.Header("Cross-Origin-Opener-Policy", "same-origin")
		c.Header("Cross-Origin-Resource-Policy", "same-origin")

		c.Next()
	}
}
