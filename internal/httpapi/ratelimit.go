package httpapi

import (
	"net"
	"net/http"
	"sync"
	"time"
)

// rateLimiter is a per-client token bucket. Bundle requests cost more than
// coarse ones, and deeper bundles cost the most (a z14 bundle is 256 upstream
// reads), so abusive fine traffic is throttled harder than ordinary panning.
//
// Time is injectable for tests. Buckets are reclaimed lazily on access; a
// background sweep bounds memory under churn.
type rateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	rate    float64 // tokens per second
	burst   float64
	now     func() time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newRateLimiter(ratePerSec, burst float64, now func() time.Time) *rateLimiter {
	if now == nil {
		now = time.Now
	}
	return &rateLimiter{
		buckets: make(map[string]*bucket),
		rate:    ratePerSec,
		burst:   burst,
		now:     now,
	}
}

// allow deducts cost tokens for client, refilling by elapsed time. It returns
// false when the client is over budget.
func (r *rateLimiter) allow(client string, cost float64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	b, ok := r.buckets[client]
	if !ok {
		b = &bucket{tokens: r.burst, last: now}
		r.buckets[client] = b
	}
	elapsed := now.Sub(b.last).Seconds()
	if elapsed > 0 {
		b.tokens += elapsed * r.rate
		if b.tokens > r.burst {
			b.tokens = r.burst
		}
		b.last = now
	}
	if b.tokens < cost {
		return false
	}
	b.tokens -= cost
	return true
}

// sweep drops buckets that have fully refilled and gone idle, bounding memory.
func (r *rateLimiter) sweep() {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	for k, b := range r.buckets {
		if now.Sub(b.last) > 10*time.Minute {
			delete(r.buckets, k)
		}
	}
}

// clientIP extracts a stable client key. It trusts the platform's socket peer;
// the API sits behind cluster ingress that sets the connection source. We
// deliberately do not parse X-Forwarded-For here to avoid client spoofing of
// the rate-limit key.
func clientIP(req *http.Request) string {
	host, _, err := net.SplitHostPort(req.RemoteAddr)
	if err != nil {
		return req.RemoteAddr
	}
	return host
}
