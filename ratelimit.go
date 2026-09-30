package main

import (
	"sync"
	"time"
)

// rate_limiter is a token bucket. Tokens accrue at rate bytes per second up to
// burst, and wait spends them, sleeping off whatever it could not pay for.
//
// A nil limiter never blocks, so callers can hold one unconditionally and the
// unlimited case costs a nil check rather than a branch at every call site.
type rate_limiter struct {
	mu     sync.Mutex
	rate   float64 // bytes per second
	burst  float64 // ceiling on banked tokens, so an idle period cannot buy an unbounded burst later
	tokens float64
	last   time.Time
}

// new_rate_limiter returns nil for a limit of zero or less, meaning unlimited.
func new_rate_limiter(bytes_per_sec int) *rate_limiter {
	if bytes_per_sec <= 0 {
		return nil
	}

	// one second of traffic, floored at a block: a limit below the block size
	// must still let a whole block through, just slowly
	burst := float64(bytes_per_sec)
	if burst < block_size {
		burst = block_size
	}

	return &rate_limiter{
		rate:   float64(bytes_per_sec),
		burst:  burst,
		tokens: burst,
		last:   time.Now(),
	}
}

// wait charges n bytes against the bucket and blocks until they are paid for.
//
// Tokens are allowed to go negative. Each caller takes what it needs, then
// sleeps off exactly its own debt, so concurrent callers queue up naturally
// instead of contending for a shared deadline. It also means a request larger
// than burst is served after a longer sleep rather than deadlocking.
func (r *rate_limiter) wait(n int) {
	if r == nil || n <= 0 {
		return
	}

	r.mu.Lock()
	now := time.Now()
	r.tokens += now.Sub(r.last).Seconds() * r.rate
	if r.tokens > r.burst {
		r.tokens = r.burst
	}
	r.last = now
	r.tokens -= float64(n)
	debt := r.tokens
	r.mu.Unlock()

	if debt < 0 {
		time.Sleep(time.Duration(-debt / r.rate * float64(time.Second)))
	}
}

// limit returns the configured rate in bytes per second, or 0 when unlimited.
// The dashboard reads this to show the cap next to the live speed.
func (r *rate_limiter) limit() int64 {
	if r == nil {
		return 0
	}
	return int64(r.rate)
}
