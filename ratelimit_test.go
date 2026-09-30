package main

import (
	"sync"
	"testing"
	"time"
)

func TestRateLimiterUnlimitedIsNil(t *testing.T) {
	for _, limit := range []int{0, -1, -4096} {
		if r := new_rate_limiter(limit); r != nil {
			t.Errorf("new_rate_limiter(%d) = %v, want nil (unlimited)", limit, r)
		}
	}
}

func TestNilLimiterNeverBlocks(t *testing.T) {
	var r *rate_limiter // unlimited

	start := time.Now()
	for i := 0; i < 1000; i++ {
		r.wait(block_size)
	}
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Errorf("nil limiter blocked for %v, want ~0", elapsed)
	}
	if r.limit() != 0 {
		t.Errorf("nil limiter limit() = %d, want 0", r.limit())
	}
}

// The first burst is free, so spend it before timing anything: what we care
// about is the sustained rate once the bucket is empty.
func TestRateLimiterThrottlesToConfiguredRate(t *testing.T) {
	const rate = 100 * 1024 // 100 KB/s
	r := new_rate_limiter(rate)

	r.wait(int(r.burst)) // drain the initial allowance

	const want_bytes = 50 * 1024 // half a second's worth
	start := time.Now()
	for sent := 0; sent < want_bytes; sent += 8192 {
		r.wait(8192)
	}
	elapsed := time.Since(start)

	expected := time.Duration(float64(want_bytes) / rate * float64(time.Second))
	// generous lower bound (timers overshoot, never undershoot by much) and a
	// loose upper bound so a busy CI machine does not fail the build
	if elapsed < expected*7/10 {
		t.Errorf("sent %d bytes in %v, too fast for a %d B/s cap (expected ~%v)", want_bytes, elapsed, rate, expected)
	}
	if elapsed > expected*3 {
		t.Errorf("sent %d bytes in %v, far slower than the %d B/s cap (expected ~%v)", want_bytes, elapsed, rate, expected)
	}
}

// Every worker shares one limiter, so the cap is a total across peers rather
// than a per-peer allowance.
func TestRateLimiterIsSharedAcrossGoroutines(t *testing.T) {
	const rate = 200 * 1024
	r := new_rate_limiter(rate)
	r.wait(int(r.burst))

	const workers = 8
	const per_worker = 16 * 1024

	start := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for sent := 0; sent < per_worker; sent += 8192 {
				r.wait(8192)
			}
		}()
	}
	wg.Wait()
	elapsed := time.Since(start)

	total := workers * per_worker
	expected := time.Duration(float64(total) / rate * float64(time.Second))
	if elapsed < expected*7/10 {
		t.Errorf("%d goroutines moved %d bytes in %v, too fast for a shared %d B/s cap (expected ~%v)",
			workers, total, elapsed, rate, expected)
	}
}

// A limit smaller than one block must still let a whole block through, just
// slowly. Without the burst floor this would deadlock.
func TestRateLimiterBelowBlockSizeStillProgresses(t *testing.T) {
	r := new_rate_limiter(1024) // 1 KB/s, far below the 16 KB block size

	done := make(chan struct{})
	go func() {
		r.wait(block_size)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("wait(block_size) deadlocked under a sub-block limit")
	}
}

func TestRateLimiterReportsItsLimit(t *testing.T) {
	r := new_rate_limiter(2048)
	if got := r.limit(); got != 2048 {
		t.Errorf("limit() = %d, want 2048", got)
	}
}

func TestFormatCap(t *testing.T) {
	if got := format_cap(nil); got != "" {
		t.Errorf("format_cap(nil) = %q, want empty", got)
	}
	if got := format_cap(new_rate_limiter(2048)); got != " (capped 2.00 KB/s)" {
		t.Errorf("format_cap(2048) = %q", got)
	}
}
