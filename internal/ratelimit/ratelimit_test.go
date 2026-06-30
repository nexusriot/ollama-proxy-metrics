package ratelimit

import (
	"testing"
	"time"
)

func TestLimiter_Disabled(t *testing.T) {
	l := New(0, time.Minute)
	if l.Enabled() {
		t.Error("limit 0 should be disabled")
	}
	for i := 0; i < 100; i++ {
		if !l.Allow("k") {
			t.Fatal("disabled limiter must always allow")
		}
	}
}

func TestLimiter_NilSafe(t *testing.T) {
	var l *Limiter
	if l.Enabled() {
		t.Error("nil limiter should report disabled")
	}
	if !l.Allow("k") {
		t.Error("nil limiter should allow")
	}
}

func TestLimiter_PerKeyWindow(t *testing.T) {
	l := New(2, time.Minute)
	// fixed clock
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	now := base
	l.now = func() time.Time { return now }

	if !l.Allow("a") || !l.Allow("a") {
		t.Fatal("first two requests should pass")
	}
	if l.Allow("a") {
		t.Error("third request in window should be blocked")
	}
	// A different key has its own bucket.
	if !l.Allow("b") {
		t.Error("different key should pass")
	}
	// Advancing past the window resets the count.
	now = base.Add(time.Minute + time.Second)
	if !l.Allow("a") {
		t.Error("request after window should pass")
	}
}
