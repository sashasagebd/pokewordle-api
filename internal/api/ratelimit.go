package api

import (
	"math"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"
)

// rateLimiter is a token bucket per key (a client, see clientKey): each holds
// up to burst tokens, refilled at rate a second, and a request takes one.
//
// It's in memory, so each running version counts separately (two at once
// during a deploy); that's fine for what it's for, keeping abuse and load in
// check. It can't stop a bot that plays within the limits, and isn't meant
// to.
type rateLimiter struct {
	rate  float64 // tokens a second
	burst float64
	now   func() time.Time

	mu        sync.Mutex
	buckets   map[string]*bucket
	lastSweep time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newRateLimiter(rate float64, burst int) *rateLimiter {
	return &rateLimiter{rate: rate, burst: float64(burst), now: time.Now, buckets: map[string]*bucket{}}
}

// perMinute and perHour turn "n per period" into a rate a second.
func perMinute(n float64) float64 { return n / 60 }
func perHour(n float64) float64   { return n / 3600 }

// allow takes a token for key. If there's none, it says how long until there
// will be.
func (l *rateLimiter) allow(key string) (ok bool, retryAfter time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.sweep(now)

	b, found := l.buckets[key]
	if !found {
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	}
	b.tokens = math.Min(l.burst, b.tokens+now.Sub(b.last).Seconds()*l.rate)
	b.last = now
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	wait := time.Duration((1 - b.tokens) / l.rate * float64(time.Second))
	return false, wait
}

// sweep forgets buckets that have refilled completely (a new one starts full
// anyway), at most once a minute, so memory follows recent clients only.
func (l *rateLimiter) sweep(now time.Time) {
	if now.Sub(l.lastSweep) < time.Minute {
		return
	}
	l.lastSweep = now
	for k, b := range l.buckets {
		if b.tokens+now.Sub(b.last).Seconds()*l.rate >= l.burst {
			delete(l.buckets, k)
		}
	}
}

// A limitGroup is a set of routes sharing limits.
type limitGroup struct {
	limiter *rateLimiter
	message string
}

// limits are the API's rate limits, per client.
type limits struct {
	newPlayers, passwords, moves, reads *limitGroup
}

func newLimits() *limits {
	return &limits{
		// Each new player is a row; a real person needs one per device, and
		// this leaves room for many people behind one address.
		newPlayers: &limitGroup{newRateLimiter(perHour(30), 10), "Too many new players from here. Try again later."},
		// Slows down password guessing.
		passwords: &limitGroup{newRateLimiter(perMinute(passwordAttemptsPerMinute), passwordAttemptsPerMinute), "Too many tries. Wait a minute and try again."},
		// Fast Higher or Lower is about a click a second.
		moves: &limitGroup{newRateLimiter(2, 20), "Slow down a moment."},
		// Pages load several things at once, and Who's That two images a round.
		reads: &limitGroup{newRateLimiter(10, 60), "Slow down a moment."},
	}
}

// Password endpoints allow this many attempts per client a minute.
const passwordAttemptsPerMinute = 10

// group returns the limits r falls under, or nil for none (the health check).
func (l *limits) group(r *http.Request) *limitGroup {
	path := r.URL.Path
	switch {
	case path == "/healthz":
		return nil
	case r.Method == http.MethodPost && path == "/players":
		return l.newPlayers
	case r.Method == http.MethodPost && (path == "/me/username" || path == "/sessions"):
		return l.passwords
	case r.Method == http.MethodGet || r.Method == http.MethodHead:
		return l.reads
	default: // starting games, moves, logging out
		return l.moves
	}
}

// middleware answers 429 to a client over its limit, with Retry-After.
func (l *limits) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g := l.group(r)
		if g == nil {
			next.ServeHTTP(w, r)
			return
		}
		if ok, wait := g.limiter.allow(clientKey(r)); !ok {
			w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(wait.Seconds()))))
			writeError(w, http.StatusTooManyRequests, g.message)
			return
		}
		next.ServeHTTP(w, r)
	})
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

// clientKey is who a request counts against: its IPv4 address, or its IPv6
// address's /64, since one connection usually has a whole /64 to pick from.
func clientKey(r *http.Request) string {
	ip := clientIP(r)
	addr, err := netip.ParseAddr(strings.TrimSpace(ip))
	if err != nil {
		return ip
	}
	addr = addr.Unmap()
	if addr.Is4() {
		return addr.String()
	}
	prefix, err := addr.Prefix(64)
	if err != nil {
		return addr.String()
	}
	return prefix.String()
}
