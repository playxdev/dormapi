package httpx

import (
	"net"
	"net/http"
	"sync"
	"time"
)

// RateLimit caps how often one client may call a route.
//
// This lives in the process because nothing else is in the path: the service
// answers on its platform hostname directly, with no proxy holding a WAF rule.
// The cost is that the limit is per instance — two instances allow twice the
// rate — and that it resets on deploy. Both are acceptable for what this
// defends: guessing an eight-character invite code, or mining
// /recovery/request for whether an address rents here. Neither is defeated by
// doubling the ceiling.
//
// A fixed window rather than a token bucket. The imprecision at a window
// boundary does not matter when the limits are this low, and the memory is one
// counter per caller rather than a timestamp per request.
type limiter struct {
	limit  int
	window time.Duration

	mu      sync.Mutex
	counts  map[string]int
	resetAt time.Time
}

func newLimiter(limit int, window time.Duration) *limiter {
	return &limiter{
		limit:   limit,
		window:  window,
		counts:  make(map[string]int),
		resetAt: time.Now().Add(window),
	}
}

func (l *limiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	// One shared window, cleared wholesale. Per-key windows would need a
	// sweeper to keep the map from growing with every address ever seen.
	if now := time.Now(); now.After(l.resetAt) {
		l.counts = make(map[string]int)
		l.resetAt = now.Add(l.window)
	}

	if l.counts[key] >= l.limit {
		return false
	}
	l.counts[key]++
	return true
}

// RateLimit rejects a caller that exceeds limit requests per window.
//
// Keyed on the client address. Behind a proxy that address is the proxy's, so
// this must not be wrapped around one without reading a forwarded header
// first — a shared key would rate-limit every tenant as if they were one.
func RateLimit(limit int, window time.Duration) func(http.Handler) http.Handler {
	l := newLimiter(limit, window)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !l.allow(clientIP(r)) {
				w.Header().Set("Retry-After", "900")
				writeError(w, http.StatusTooManyRequests, "rate_limited")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
