package api

import (
	"net"
	"net/http"
	"sync"
	"time"
)

// rateLimiter allows each key (a client IP) n requests per window. It's in
// memory, so each running version counts separately (two at once during a
// deploy); that's fine for slowing down password guessing, which is all it's
// for. Phase 4 brings rate limits for the rest of the API.
type rateLimiter struct {
	n      int
	window time.Duration
	now    func() time.Time

	mu   sync.Mutex
	hits map[string]*windowCount
}

type windowCount struct {
	start time.Time
	count int
}

func newRateLimiter(n int, window time.Duration) *rateLimiter {
	return &rateLimiter{n: n, window: window, now: time.Now, hits: map[string]*windowCount{}}
}

// allow counts a request from key and reports whether it's within the limit.
func (l *rateLimiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if len(l.hits) > 10_000 { // forget finished windows now and then
		for k, w := range l.hits {
			if now.Sub(w.start) >= l.window {
				delete(l.hits, k)
			}
		}
	}
	w, ok := l.hits[key]
	if !ok || now.Sub(w.start) >= l.window {
		l.hits[key] = &windowCount{start: now, count: 1}
		return true
	}
	w.count++
	return w.count <= l.n
}

// clientIP is the address the request came from. Requests from the internet
// arrive through Cloudflare, which sets CF-Connecting-IP (and replaces any the
// client sent); otherwise it's the peer's address.
func clientIP(r *http.Request) string {
	if ip := r.Header.Get("CF-Connecting-IP"); ip != "" {
		return ip
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
