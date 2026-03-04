package http

import (
	"context"

	"golang.org/x/time/rate"
)

// RateLimiter wraps a token bucket rate limiter to control request throughput.
type RateLimiter struct {
	limiter *rate.Limiter
}

// NewRateLimiter creates a rate limiter that allows requestsPerSecond sustained
// rate with the given burst size for short bursts above the sustained rate.
func NewRateLimiter(requestsPerSecond float64, burst int) *RateLimiter {
	return &RateLimiter{
		limiter: rate.NewLimiter(rate.Limit(requestsPerSecond), burst),
	}
}

// Wait blocks until the rate limiter permits one more request, or the context
// is cancelled. Returns an error if the context is cancelled or its deadline
// is exceeded before the limiter grants permission.
func (rl *RateLimiter) Wait(ctx context.Context) error {
	return rl.limiter.Wait(ctx)
}
