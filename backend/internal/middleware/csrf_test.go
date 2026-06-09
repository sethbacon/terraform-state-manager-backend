package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func init() { gin.SetMode(gin.TestMode) }

// newCSRFRequest builds a request with the given method and optional auth cookie,
// CSRF cookie, and CSRF header.
func newCSRFRequest(method, authCookie, csrfCookie, csrfHeader string) *http.Request {
	req := httptest.NewRequest(method, "/api/v1/sources", nil)
	if authCookie != "" {
		req.AddCookie(&http.Cookie{Name: AuthCookieName, Value: authCookie})
	}
	if csrfCookie != "" {
		req.AddCookie(&http.Cookie{Name: CSRFCookieName, Value: csrfCookie})
	}
	if csrfHeader != "" {
		req.Header.Set(CSRFHeaderName, csrfHeader)
	}
	return req
}

func runCSRF(req *http.Request) int {
	w := httptest.NewRecorder()
	r := gin.New()
	r.Use(CSRFProtect())
	r.Handle(req.Method, "/api/v1/sources", func(c *gin.Context) { c.Status(http.StatusOK) })
	r.ServeHTTP(w, req)
	return w.Code
}

func TestCSRFProtect(t *testing.T) {
	cases := []struct {
		name       string
		method     string
		authCookie string
		csrfCookie string
		csrfHeader string
		want       int
	}{
		{"GET always allowed", http.MethodGet, "jwt", "", "", http.StatusOK},
		{"mutating without auth cookie passes (not CSRF-eligible)", http.MethodPost, "", "", "", http.StatusOK},
		{"cookie-auth POST missing header rejected", http.MethodPost, "jwt", "tok", "", http.StatusForbidden},
		{"cookie-auth POST mismatched header rejected", http.MethodPost, "jwt", "tok", "other", http.StatusForbidden},
		{"cookie-auth POST missing cookie rejected", http.MethodPost, "jwt", "", "tok", http.StatusForbidden},
		{"cookie-auth POST matching token allowed", http.MethodPost, "jwt", "tok", "tok", http.StatusOK},
		{"cookie-auth DELETE matching token allowed", http.MethodDelete, "jwt", "tok", "tok", http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := runCSRF(newCSRFRequest(tc.method, tc.authCookie, tc.csrfCookie, tc.csrfHeader))
			if got != tc.want {
				t.Fatalf("status = %d, want %d", got, tc.want)
			}
		})
	}
}
