package api

import (
	"net/http/httptest"
	"testing"
	"time"
)

func TestTokenBucket(t *testing.T) {
	now := time.Unix(0, 0)
	l := newRateLimiter(2, 3) // 2 a second, bursts of 3
	l.now = func() time.Time { return now }

	for i := range 3 {
		if ok, _ := l.allow("a"); !ok {
			t.Fatalf("request %d of the burst refused", i+1)
		}
	}
	ok, wait := l.allow("a")
	if ok || wait != 500*time.Millisecond {
		t.Errorf("past the burst: ok %v, wait %v; want refused for 500ms", ok, wait)
	}
	if ok, _ := l.allow("b"); !ok {
		t.Error("another client is limited too")
	}

	now = now.Add(500 * time.Millisecond) // one token back
	if ok, _ := l.allow("a"); !ok {
		t.Error("refused after a token came back")
	}
	if ok, _ := l.allow("a"); ok {
		t.Error("allowed with no token left")
	}

	now = now.Add(time.Hour) // never more than the burst
	for i := range 4 {
		if ok, _ := l.allow("a"); ok != (i < 3) {
			t.Errorf("after an hour, request %d: ok %v", i+1, ok)
		}
	}
}

func TestSweepForgetsFullBuckets(t *testing.T) {
	now := time.Unix(0, 0)
	l := newRateLimiter(1, 5)
	l.now = func() time.Time { return now }
	l.allow("idle")
	for range 5 {
		l.allow("busy")
	}
	now = now.Add(2 * time.Minute) // idle is full again; so is busy, after 5s
	l.allow("new")
	if _, ok := l.buckets["idle"]; ok {
		t.Error("a full bucket was kept")
	}
	if len(l.buckets) != 1 {
		t.Errorf("%d buckets left, want just the new one", len(l.buckets))
	}
}

func TestClientKey(t *testing.T) {
	for _, c := range []struct{ remote, cf, want string }{
		{"10.0.0.5:1234", "", "10.0.0.5"},
		{"10.0.0.5:1234", "203.0.113.7", "203.0.113.7"},
		{"10.0.0.5:1234", "2001:db8:aa:bb:1:2:3:4", "2001:db8:aa:bb::/64"},
		{"10.0.0.5:1234", "2001:db8:aa:bb:ffff::1", "2001:db8:aa:bb::/64"}, // same /64, same client
		{"10.0.0.5:1234", "::ffff:198.51.100.9", "198.51.100.9"},
		{"[2001:db8::1]:443", "", "2001:db8::/64"},
	} {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = c.remote
		if c.cf != "" {
			r.Header.Set("CF-Connecting-IP", c.cf)
		}
		if got := clientKey(r); got != c.want {
			t.Errorf("remote %s, CF %q: key %q, want %q", c.remote, c.cf, got, c.want)
		}
	}
}

func TestRoutesFallInTheirGroups(t *testing.T) {
	l := newLimits()
	for _, c := range []struct {
		method, path string
		want         *limitGroup
	}{
		{"GET", "/healthz", nil},
		{"POST", "/players", l.newPlayers},
		{"POST", "/sessions", l.passwords},
		{"POST", "/me/username", l.passwords},
		{"POST", "/games", l.moves},
		{"POST", "/games/x/answer", l.moves},
		{"DELETE", "/sessions/current", l.moves},
		{"GET", "/pokemon", l.reads},
		{"GET", "/games/x/rounds/1/image", l.reads},
		{"GET", "/leaderboard", l.reads},
	} {
		if got := l.group(httptest.NewRequest(c.method, c.path, nil)); got != c.want {
			t.Errorf("%s %s: wrong group", c.method, c.path)
		}
	}
}

func TestTooManyRequests(t *testing.T) {
	h := newTestAPI(nil)
	origin := map[string]string{"Origin": frontend}
	for i := range 60 { // the reads' burst
		if code := do(h, "GET", "/pokemon", origin).Code; code != 200 {
			t.Fatalf("read %d: got %d", i+1, code)
		}
	}
	rec := do(h, "GET", "/pokemon", origin)
	if rec.Code != 429 || rec.Header().Get("Retry-After") != "1" {
		t.Errorf("read 61: got %d, Retry-After %q; want 429 and 1", rec.Code, rec.Header().Get("Retry-After"))
	}
	if rec.Header().Get("Access-Control-Allow-Origin") != frontend {
		t.Error("a 429 must carry CORS headers, or the frontend can't read it")
	}
	if code := do(h, "GET", "/healthz", nil).Code; code != 200 {
		t.Errorf("health check limited: %d", code)
	}
}
