package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func applySecurityHeadersCfg(cfg SecurityHeadersConfig) *httptest.ResponseRecorder {
	r := gin.New()
	r.Use(SecurityHeadersMiddleware(cfg))
	r.GET("/", func(c *gin.Context) { c.Status(http.StatusOK) })
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	return w
}

func applySecurityHeadersTLS(cfg SecurityHeadersConfig) *httptest.ResponseRecorder {
	r := gin.New()
	r.Use(SecurityHeadersMiddleware(cfg))
	r.GET("/", func(c *gin.Context) { c.Status(http.StatusOK) })
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Forwarded-Proto", "https")
	r.ServeHTTP(w, req)
	return w
}

func TestAPISecurityHeadersConfig(t *testing.T) {
	cfg := APISecurityHeadersConfig()
	if !cfg.EnableHSTS {
		t.Error("EnableHSTS = false, want true")
	}
	if cfg.EnableXSSProtection {
		t.Error("EnableXSSProtection = true, want false for API")
	}
	if cfg.ContentSecurityPolicy == "" {
		t.Error("ContentSecurityPolicy empty")
	}
	if cfg.ReferrerPolicy != "no-referrer" {
		t.Errorf("ReferrerPolicy = %q, want no-referrer", cfg.ReferrerPolicy)
	}
}

func TestSecurityHeadersMiddleware_HSTS(t *testing.T) {
	t.Run("sent over TLS", func(t *testing.T) {
		cfg := SecurityHeadersConfig{EnableHSTS: true, HSTSMaxAge: 31536000, HSTSIncludeSubdomains: true}
		w := applySecurityHeadersTLS(cfg)
		hsts := w.Header().Get("Strict-Transport-Security")
		if !strings.Contains(hsts, "max-age=31536000") {
			t.Errorf("HSTS = %q, want max-age=31536000", hsts)
		}
		if !strings.Contains(hsts, "includeSubDomains") {
			t.Errorf("HSTS = %q, want includeSubDomains", hsts)
		}
	})
	t.Run("preload", func(t *testing.T) {
		cfg := SecurityHeadersConfig{EnableHSTS: true, HSTSMaxAge: 86400, HSTSPreload: true}
		w := applySecurityHeadersTLS(cfg)
		if !strings.Contains(w.Header().Get("Strict-Transport-Security"), "preload") {
			t.Error("HSTS missing preload")
		}
	})
	t.Run("not sent over plain HTTP", func(t *testing.T) {
		cfg := SecurityHeadersConfig{EnableHSTS: true, HSTSMaxAge: 31536000}
		w := applySecurityHeadersCfg(cfg)
		if got := w.Header().Get("Strict-Transport-Security"); got != "" {
			t.Errorf("HSTS should not be sent over plain HTTP, got %q", got)
		}
	})
	t.Run("disabled", func(t *testing.T) {
		w := applySecurityHeadersTLS(SecurityHeadersConfig{EnableHSTS: false})
		if got := w.Header().Get("Strict-Transport-Security"); got != "" {
			t.Errorf("HSTS should be absent when disabled, got %q", got)
		}
	})
}

func TestSecurityHeadersMiddleware_FrameOptions(t *testing.T) {
	t.Run("DENY", func(t *testing.T) {
		w := applySecurityHeadersCfg(SecurityHeadersConfig{EnableFrameOptions: true, FrameOptionsValue: "DENY"})
		if got := w.Header().Get("X-Frame-Options"); got != "DENY" {
			t.Errorf("X-Frame-Options = %q, want DENY", got)
		}
	})
	t.Run("disabled", func(t *testing.T) {
		w := applySecurityHeadersCfg(SecurityHeadersConfig{EnableFrameOptions: false, FrameOptionsValue: "DENY"})
		if got := w.Header().Get("X-Frame-Options"); got != "" {
			t.Errorf("X-Frame-Options should be absent, got %q", got)
		}
	})
}

func TestSecurityHeadersMiddleware_CSP(t *testing.T) {
	w := applySecurityHeadersCfg(SecurityHeadersConfig{ContentSecurityPolicy: "default-src 'none'"})
	if got := w.Header().Get("Content-Security-Policy"); got != "default-src 'none'" {
		t.Errorf("CSP = %q, want default-src 'none'", got)
	}
	w = applySecurityHeadersCfg(SecurityHeadersConfig{ContentSecurityPolicy: ""})
	if got := w.Header().Get("Content-Security-Policy"); got != "" {
		t.Errorf("CSP should be absent when empty, got %q", got)
	}
}

func TestSecurityHeadersMiddleware_FixedHeaders(t *testing.T) {
	w := applySecurityHeadersCfg(SecurityHeadersConfig{})
	tests := []struct{ header, want string }{
		{"X-Permitted-Cross-Domain-Policies", "none"},
		{"Cross-Origin-Embedder-Policy", "require-corp"},
		{"Cross-Origin-Opener-Policy", "same-origin"},
		{"Cross-Origin-Resource-Policy", "same-origin"},
	}
	for _, tt := range tests {
		if got := w.Header().Get(tt.header); got != tt.want {
			t.Errorf("%s = %q, want %q", tt.header, got, tt.want)
		}
	}
}
