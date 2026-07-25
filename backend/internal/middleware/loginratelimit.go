// loginratelimit.go throttles password-login endpoints at the application layer.
// It mirrors the setup-token limiter (a per-IP sliding window) so the only
// endpoint where the app itself verifies a primary credential — LDAP login — has
// brute-force protection independent of any proxy/gateway limit (#250). Kept
// separate from the setup limiter so the two paths tune independently.
package middleware

import (
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

const (
	loginMaxAttempts = 5
	loginRateWindow  = time.Minute
)

// loginRateLimiter is a per-key sliding-window attempt counter.
type loginRateLimiter struct {
	mu       sync.Mutex
	attempts map[string][]time.Time
	max      int
	window   time.Duration
}

func newLoginRateLimiter(max int, window time.Duration) *loginRateLimiter {
	return &loginRateLimiter{attempts: make(map[string][]time.Time), max: max, window: window}
}

// allow records an attempt for key and reports whether it is within the window
// budget. Old attempts outside the window are pruned on each call.
func (rl *loginRateLimiter) allow(key string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	cutoff := time.Now().Add(-rl.window)
	recent := make([]time.Time, 0, len(rl.attempts[key]))
	for _, t := range rl.attempts[key] {
		if t.After(cutoff) {
			recent = append(recent, t)
		}
	}
	if len(recent) >= rl.max {
		rl.attempts[key] = recent
		return false
	}
	rl.attempts[key] = append(recent, time.Now())
	return true
}

// LoginRateLimit throttles a password-login route per client IP
// (loginMaxAttempts per loginRateWindow), aborting with 429 before any
// credential check so online brute-force is blunted at the application layer.
// Per-IP only by design: a per-username lockout would let an attacker lock out
// legitimate users (an availability DoS the finding itself calls out).
func LoginRateLimit() gin.HandlerFunc {
	rl := newLoginRateLimiter(loginMaxAttempts, loginRateWindow)
	return func(c *gin.Context) {
		if !rl.allow(c.ClientIP()) {
			slog.Warn("login rate limit exceeded", "ip", c.ClientIP(), "path", c.FullPath())
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
				"error": "too many login attempts; try again in one minute",
			})
			return
		}
		c.Next()
	}
}
