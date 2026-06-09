// csrf.go implements the double-submit CSRF cookie pattern. A non-HttpOnly
// "tsm_csrf" cookie is set alongside the auth cookie so the frontend can read it
// and echo it in the X-CSRF-Token header on mutating requests; CSRFProtect then
// verifies the two match for cookie-authenticated, state-changing requests.
package middleware

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"net/http"

	"github.com/gin-gonic/gin"
)

// CSRFCookieName is the non-HttpOnly cookie readable by the frontend.
const CSRFCookieName = "tsm_csrf"

// CSRFHeaderName is the header the frontend echoes the cookie value in.
const CSRFHeaderName = "X-CSRF-Token"

// CSRFProtect enforces the double-submit check on state-changing requests that
// are authenticated by the session cookie: the client must echo the tsm_csrf
// cookie value in X-CSRF-Token. Requests without the auth cookie — machine
// callbacks carrying a per-run token, or bearer/API-key clients that browsers
// cannot drive cross-site — are not CSRF-eligible and pass through. Safe methods
// are always allowed.
func CSRFProtect() gin.HandlerFunc {
	return func(c *gin.Context) {
		switch c.Request.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace:
			c.Next()
			return
		}
		// Only requests carrying the session cookie can be forged cross-site.
		if cookie, err := c.Cookie(AuthCookieName); err != nil || cookie == "" {
			c.Next()
			return
		}
		cookieTok, err := c.Cookie(CSRFCookieName)
		header := c.GetHeader(CSRFHeaderName)
		if err != nil || cookieTok == "" || header == "" ||
			subtle.ConstantTimeCompare([]byte(cookieTok), []byte(header)) != 1 {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "CSRF token missing or invalid"})
			return
		}
		c.Next()
	}
}

func generateCSRFToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// SetCSRFCookie sets a fresh double-submit CSRF cookie and returns its token.
func SetCSRFCookie(w http.ResponseWriter, secure bool) (string, error) {
	token, err := generateCSRFToken()
	if err != nil {
		return "", err
	}
	// #nosec G124 -- CSRF double-submit cookie must be readable by JS (HttpOnly:false
	// by design) so the SPA can echo it in X-CSRF-Token; Secure is config-derived so
	// it is still sent over http://localhost in dev and forced on in production.
	http.SetCookie(w, &http.Cookie{
		Name:     CSRFCookieName,
		Value:    token,
		Path:     "/",
		MaxAge:   86400,
		Secure:   secure,
		HttpOnly: false, // must be readable by JS
		SameSite: http.SameSiteLaxMode,
	})
	return token, nil
}

// ClearCSRFCookie removes the CSRF cookie (e.g. on logout).
func ClearCSRFCookie(w http.ResponseWriter) {
	// #nosec G124 -- expiring the JS-readable CSRF cookie; HttpOnly:false mirrors
	// how it was set (double-submit pattern), value is emptied with MaxAge -1.
	http.SetCookie(w, &http.Cookie{
		Name:     CSRFCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		Secure:   true,
		HttpOnly: false,
		SameSite: http.SameSiteLaxMode,
	})
}
