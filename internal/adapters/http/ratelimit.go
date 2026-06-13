package http

import (
	"net/http"
	"sync"
	"time"
)

// rateLimiter is a small concurrency-safe token-bucket limiter keyed by an
// arbitrary string (tenant id or client IP). It bounds load on expensive
// endpoints (threat H3/W4). The clock is injectable for deterministic tests.
type rateLimiter struct {
	mu       sync.Mutex
	buckets  map[string]*bucket
	capacity float64
	refill   float64 // tokens per second
	now      func() time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

// newRateLimiter builds a limiter allowing bursts up to capacity, refilling at
// refillPerSec tokens/second.
func newRateLimiter(capacity, refillPerSec float64, now func() time.Time) *rateLimiter {
	if now == nil {
		now = time.Now
	}
	return &rateLimiter{
		buckets:  make(map[string]*bucket),
		capacity: capacity,
		refill:   refillPerSec,
		now:      now,
	}
}

// allow reports whether a request keyed by key may proceed, consuming a token.
func (rl *rateLimiter) allow(key string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	t := rl.now()
	b, ok := rl.buckets[key]
	if !ok {
		rl.buckets[key] = &bucket{tokens: rl.capacity - 1, last: t}
		return true
	}
	elapsed := t.Sub(b.last).Seconds()
	b.tokens += elapsed * rl.refill
	if b.tokens > rl.capacity {
		b.tokens = rl.capacity
	}
	b.last = t
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// middleware limits by the key returned by keyFn (e.g. tenant id, or remote IP).
func (rl *rateLimiter) middleware(keyFn func(*http.Request) string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !rl.allow(keyFn(r)) {
				writeError(w, http.StatusTooManyRequests, "rate limit exceeded")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// tenantOrIPKey keys by authenticated tenant when present, else remote address.
func tenantOrIPKey(r *http.Request) string {
	if tid := tenantFromContext(r.Context()); tid != "" {
		return "t:" + tid
	}
	return "ip:" + clientIP(r)
}

func clientIP(r *http.Request) string {
	// RemoteAddr is host:port; strip the port. Trusting proxy headers is a
	// deployment concern handled at the ingress, not here.
	addr := r.RemoteAddr
	for i := len(addr) - 1; i >= 0; i-- {
		if addr[i] == ':' {
			return addr[:i]
		}
	}
	return addr
}
