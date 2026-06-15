// setup.go authenticates first-run setup-wizard requests. Setup endpoints use a
// separate scheme ("Authorization: SetupToken <token>") independent of the
// normal session/API-key chain. The token is generated once at first boot and
// invalidated when setup completes, permanently disabling these endpoints.
package middleware

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
)

// SetupTokenContextKey is set on the gin context when a request authenticates
// via the first-run setup token.
const SetupTokenContextKey = "is_setup_request"

// setupStatusStore is the subset of the system-settings repository the setup
// middleware needs. Kept as an interface so the middleware package does not
// import repositories (avoiding a cycle) and is trivially mockable in tests.
type setupStatusStore interface {
	IsSetupCompleted(context.Context) (bool, error)
	HasPendingFeatureSetup(context.Context) (bool, error)
	GetSetupTokenHash(context.Context) (string, error)
}

const (
	setupMaxAttempts = 5
	setupRateWindow  = time.Minute
)

// setupRateLimiter tracks per-IP attempt counts to blunt brute-force attacks on
// the setup token: at most setupMaxAttempts per setupRateWindow per IP.
type setupRateLimiter struct {
	mu       sync.Mutex
	attempts map[string][]time.Time
}

func newSetupRateLimiter() *setupRateLimiter {
	return &setupRateLimiter{attempts: make(map[string][]time.Time)}
}

func (rl *setupRateLimiter) allow(ip string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	cutoff := time.Now().Add(-setupRateWindow)
	recent := make([]time.Time, 0, len(rl.attempts[ip]))
	for _, t := range rl.attempts[ip] {
		if t.After(cutoff) {
			recent = append(recent, t)
		}
	}
	if len(recent) >= setupMaxAttempts {
		rl.attempts[ip] = recent
		return false
	}
	rl.attempts[ip] = append(recent, time.Now())
	return true
}

// SetupTokenMiddleware validates setup-token auth:
//  1. 403 if setup is already completed (unless a later-release feature is pending).
//  2. per-IP rate limit (before any bcrypt work).
//  3. "Authorization: SetupToken <token>" header.
//  4. bcrypt match against the hash in system_settings.
//
// On success it sets SetupTokenContextKey=true and calls c.Next().
func SetupTokenMiddleware(store setupStatusStore) gin.HandlerFunc {
	rateLimiter := newSetupRateLimiter()

	return func(c *gin.Context) {
		ctx := c.Request.Context()

		completed, err := store.IsSetupCompleted(ctx)
		if err != nil {
			slog.Error("setup middleware: failed to check setup status", "error", err)
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "failed to check setup status"})
			return
		}
		if completed {
			hasPending, pendingErr := store.HasPendingFeatureSetup(ctx)
			if pendingErr != nil {
				slog.Error("setup middleware: failed to check pending features", "error", pendingErr)
				c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "failed to check setup status"})
				return
			}
			if !hasPending {
				c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
					"error": "setup has already been completed; these endpoints are permanently disabled",
				})
				return
			}
		}

		clientIP := c.ClientIP()
		if !rateLimiter.allow(clientIP) {
			slog.Warn("setup middleware: rate limit exceeded", "ip", clientIP)
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
				"error": "too many setup token attempts; try again in one minute",
			})
			return
		}

		authHeader := c.GetHeader("Authorization")
		parts := strings.SplitN(authHeader, " ", 2)
		if authHeader == "" || len(parts) != 2 || !strings.EqualFold(parts[0], "SetupToken") {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"error": "Authorization header required: SetupToken <token>",
			})
			return
		}
		rawToken := strings.TrimSpace(parts[1])

		storedHash, err := store.GetSetupTokenHash(ctx)
		if err != nil {
			slog.Error("setup middleware: failed to get token hash", "error", err)
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "failed to validate setup token"})
			return
		}
		if storedHash == "" {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
				"error": "no setup token has been generated; restart the server to generate one",
			})
			return
		}
		if err := bcrypt.CompareHashAndPassword([]byte(storedHash), []byte(rawToken)); err != nil {
			slog.Warn("setup middleware: invalid setup token", "ip", clientIP)
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid setup token"})
			return
		}

		c.Set(SetupTokenContextKey, true)
		c.Next()
	}
}
