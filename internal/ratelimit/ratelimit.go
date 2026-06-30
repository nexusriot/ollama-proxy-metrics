// Package ratelimit provides a small per-key fixed-window request limiter used
// to cap how many requests a single session may make per minute.
//
// It is intentionally dependency-free and approximate: a fixed window is simpler
// than a sliding window or token bucket and is sufficient for protecting a local
// Ollama from a runaway client. A limit of 0 disables limiting entirely.
package ratelimit

import (
	"sync"
	"time"
)

// nowFunc is overridable in tests; production uses time.Now.
type nowFunc func() time.Time

// Limiter caps requests per key within a fixed time window.
type Limiter struct {
	limit  int
	window time.Duration
	now    nowFunc

	mu      sync.Mutex
	buckets map[string]*bucket
}

type bucket struct {
	windowStart time.Time
	count       int
}

// New returns a Limiter allowing limit requests per key per window. If limit <= 0
// the limiter is disabled (Allow always returns true).
func New(limit int, window time.Duration) *Limiter {
	return &Limiter{
		limit:   limit,
		window:  window,
		now:     time.Now,
		buckets: make(map[string]*bucket),
	}
}

// Enabled reports whether limiting is active.
func (l *Limiter) Enabled() bool { return l != nil && l.limit > 0 }

// Allow records a request for key and reports whether it is within the limit.
func (l *Limiter) Allow(key string) bool {
	if !l.Enabled() {
		return true
	}
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()

	b, ok := l.buckets[key]
	if !ok || now.Sub(b.windowStart) >= l.window {
		l.buckets[key] = &bucket{windowStart: now, count: 1}
		return true
	}
	if b.count >= l.limit {
		return false
	}
	b.count++
	return true
}
