package api

import (
	"net/http/httptest"
	"testing"
	"time"
)

func TestRateLimiter(t *testing.T) {
	now := time.Unix(0, 0)
	l := newRateLimiter(3, time.Minute)
	l.now = func() time.Time { return now }

	for i := range 3 {
		if !l.allow("a") {
			t.Fatalf("request %d refused", i+1)
		}
	}
	if l.allow("a") {
		t.Error("fourth request in the window allowed")
	}
	if !l.allow("b") {
		t.Error("another IP is limited too")
	}
	now = now.Add(time.Minute)
	if !l.allow("a") {
		t.Error("still refused after the window")
	}
}

func TestClientIP(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.0.0.5:1234"
	if got := clientIP(r); got != "10.0.0.5" {
		t.Errorf("without Cloudflare: %q", got)
	}
	r.Header.Set("CF-Connecting-IP", "203.0.113.7")
	if got := clientIP(r); got != "203.0.113.7" {
		t.Errorf("through Cloudflare: %q", got)
	}
}
