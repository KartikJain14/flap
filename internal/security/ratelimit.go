package security

import (
	"time"
)

// TokenBucket is a classic token bucket. It is not safe for concurrent use;
// callers provide their own locking.
type TokenBucket struct {
	rate   float64 // tokens per second
	burst  float64
	tokens float64
	last   time.Time
}

// NewTokenBucket returns a full bucket refilling at rate tokens per second.
func NewTokenBucket(rate float64, burst int, now time.Time) *TokenBucket {
	return &TokenBucket{rate: rate, burst: float64(burst), tokens: float64(burst), last: now}
}

func (b *TokenBucket) refill(now time.Time) {
	if now.After(b.last) {
		b.tokens = min(b.burst, b.tokens+now.Sub(b.last).Seconds()*b.rate)
		b.last = now
	}
}

// Allow takes one token if available.
func (b *TokenBucket) Allow(now time.Time) bool {
	b.refill(now)
	if b.tokens >= 1 {
		b.tokens--
		return true
	}
	return false
}

// Take removes n tokens, allowing the balance to go negative, and returns
// how long the caller should wait before the balance is non-negative again.
// It is used to pace a stream rather than reject it.
func (b *TokenBucket) Take(n int, now time.Time) time.Duration {
	b.refill(now)
	b.tokens -= float64(n)
	// Bound the debt so a single huge read cannot stall a reader forever.
	b.tokens = max(b.tokens, -b.burst)
	if b.tokens >= 0 || b.rate <= 0 {
		return 0
	}
	return time.Duration(-b.tokens / b.rate * float64(time.Second))
}

// Full reports whether the bucket has refilled completely, meaning its state
// carries no information and it can be forgotten.
func (b *TokenBucket) Full(now time.Time) bool {
	b.refill(now)
	return b.tokens >= b.burst
}
