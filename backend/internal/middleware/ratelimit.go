package middleware

import (
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

// RateLimitConfig holds tunables for the token-bucket rate limiter.
type RateLimitConfig struct {
	// RequestsPerMinute is the sustained request rate allowed per key.
	RequestsPerMinute int

	// BurstSize is the maximum number of tokens (requests) that can
	// accumulate, allowing short bursts above the sustained rate.
	BurstSize int

	// CleanupInterval controls how often stale bucket entries are pruned.
	CleanupInterval time.Duration
}

// DefaultRateLimitConfig returns a configuration suitable for general API
// traffic: 200 requests/min with bursts of 50.
func DefaultRateLimitConfig() RateLimitConfig {
	return RateLimitConfig{
		RequestsPerMinute: 200,
		BurstSize:         50,
		CleanupInterval:   5 * time.Minute,
	}
}

// AuthRateLimitConfig returns a stricter configuration for authentication
// endpoints: 10 requests/min with bursts of 5.
func AuthRateLimitConfig() RateLimitConfig {
	return RateLimitConfig{
		RequestsPerMinute: 10,
		BurstSize:         5,
		CleanupInterval:   5 * time.Minute,
	}
}

// UploadRateLimitConfig returns a configuration for upload endpoints:
// 30 requests/min with bursts of 5.
func UploadRateLimitConfig() RateLimitConfig {
	return RateLimitConfig{
		RequestsPerMinute: 30,
		BurstSize:         5,
		CleanupInterval:   5 * time.Minute,
	}
}

// rateLimitEntry tracks a single client's token bucket state.
type rateLimitEntry struct {
	tokens     float64
	lastRefill time.Time
}

// RateLimiter implements a per-key token-bucket rate limiter with background
// cleanup of stale entries.
type RateLimiter struct {
	mu      sync.Mutex
	entries map[string]*rateLimitEntry
	cfg     RateLimitConfig
	stopCh  chan struct{}
}

// NewRateLimiter creates a RateLimiter and starts its background cleanup
// goroutine.
func NewRateLimiter(cfg RateLimitConfig) *RateLimiter {
	rl := &RateLimiter{
		entries: make(map[string]*rateLimitEntry),
		cfg:     cfg,
		stopCh:  make(chan struct{}),
	}

	go rl.cleanup()
	return rl
}

// Stop terminates the background cleanup goroutine.
func (rl *RateLimiter) Stop() {
	close(rl.stopCh)
}

// Allow reports whether the given key is permitted to proceed. It consumes
// one token on success and returns false when the bucket is empty.
func (rl *RateLimiter) Allow(key string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	entry, ok := rl.entries[key]
	if !ok {
		entry = &rateLimitEntry{
			tokens:     float64(rl.cfg.BurstSize),
			lastRefill: now,
		}
		rl.entries[key] = entry
	}

	// Refill tokens based on elapsed time.
	elapsed := now.Sub(entry.lastRefill).Seconds()
	refillRate := float64(rl.cfg.RequestsPerMinute) / 60.0
	entry.tokens += elapsed * refillRate
	if entry.tokens > float64(rl.cfg.BurstSize) {
		entry.tokens = float64(rl.cfg.BurstSize)
	}
	entry.lastRefill = now

	if entry.tokens < 1.0 {
		return false
	}

	entry.tokens--
	return true
}

// RemainingTokens returns the current number of available tokens for the key
// without consuming any.
func (rl *RateLimiter) RemainingTokens(key string) int {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	entry, ok := rl.entries[key]
	if !ok {
		return rl.cfg.BurstSize
	}

	now := time.Now()
	elapsed := now.Sub(entry.lastRefill).Seconds()
	refillRate := float64(rl.cfg.RequestsPerMinute) / 60.0
	tokens := entry.tokens + elapsed*refillRate
	if tokens > float64(rl.cfg.BurstSize) {
		tokens = float64(rl.cfg.BurstSize)
	}

	return int(tokens)
}

// cleanup periodically removes entries that have not been seen for a while.
func (rl *RateLimiter) cleanup() {
	ticker := time.NewTicker(rl.cfg.CleanupInterval)
	defer ticker.Stop()

	for {
		select {
		case <-rl.stopCh:
			return
		case <-ticker.C:
			rl.mu.Lock()
			cutoff := time.Now().Add(-rl.cfg.CleanupInterval)
			for key, entry := range rl.entries {
				if entry.lastRefill.Before(cutoff) {
					delete(rl.entries, key)
				}
			}
			rl.mu.Unlock()
		}
	}
}

// RateLimitMiddleware returns a gin.HandlerFunc that enforces per-client
// rate limits using `rl`. When the limit is exceeded the handler replies
// with 429 Too Many Requests and sets standard X-RateLimit-* headers.
func RateLimitMiddleware(rl *RateLimiter) gin.HandlerFunc {
	return func(c *gin.Context) {
		key := getRateLimitKey(c)

		// Always set informational headers.
		c.Header("X-RateLimit-Limit", strconv.Itoa(rl.cfg.RequestsPerMinute))
		c.Header("X-RateLimit-Remaining", strconv.Itoa(rl.RemainingTokens(key)))

		if !rl.Allow(key) {
			retryAfter := 60.0 / float64(rl.cfg.RequestsPerMinute)
			c.Header("Retry-After", strconv.Itoa(int(retryAfter)+1))
			c.Header("X-RateLimit-Remaining", "0")

			slog.Warn("TSM rate limit exceeded",
				"key", key,
				"path", c.Request.URL.Path,
			)

			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
				"error":   "rate limit exceeded",
				"message": "Too many requests. Please try again later.",
			})
			return
		}

		// Update remaining after consumption.
		c.Header("X-RateLimit-Remaining", strconv.Itoa(rl.RemainingTokens(key)))
		c.Next()
	}
}

// getRateLimitKey determines the rate-limit bucket key for the current
// request. It uses the most specific identifier available:
//
//	user_id > api_key_id > client IP
func getRateLimitKey(c *gin.Context) string {
	if userID, ok := c.Get("user_id"); ok {
		if id, ok := userID.(string); ok && id != "" {
			return "user:" + id
		}
	}

	if apiKeyID, ok := c.Get("api_key_id"); ok {
		if id, ok := apiKeyID.(string); ok && id != "" {
			return "apikey:" + id
		}
	}

	return "ip:" + c.ClientIP()
}
