// Package budget enforces per-key daily token and cost ceilings.
//
// The request-per-minute limiter protects the upstream from a runaway client,
// but says nothing about spend: a hundred slow requests with 100k-token prompts
// pass a 60 rpm limit untouched. A budget caps the quantity that actually
// matters — tokens, and the cost the pricing table puts on them — over a UTC
// calendar day.
//
// Accounting is deliberately approximate in one direction: a request's usage is
// only known once it finishes, so the request that crosses the line is allowed
// and the next one is refused. Counters live in memory and are seeded from
// SQLite at startup, so a restart mid-day resumes rather than resets.
package budget

import (
	"sync"
	"time"
)

// DayFormat is the layout of the UTC calendar day used as the accounting window.
// It matches the day prefix of the RFC3339 timestamps stored in SQLite.
const DayFormat = "2006-01-02"

// Reason names the ceiling a key has crossed.
type Reason string

// The reasons Check can report.
const (
	ReasonNone   Reason = ""
	ReasonTokens Reason = "tokens_per_day"
	ReasonCost   Reason = "cost_per_day"
)

// Limits are the per-key daily ceilings. A zero value means unlimited.
type Limits struct {
	TokensPerDay int64   `json:"tokens_per_day"`
	CostPerDay   float64 `json:"cost_per_day"`
}

// Usage is one key's consumption within the current day.
type Usage struct {
	Tokens int64   `json:"tokens"`
	Cost   float64 `json:"cost"`
}

// Tracker accumulates per-key usage for the current UTC day and compares it
// against Limits.
type Tracker struct {
	limits Limits
	now    func() time.Time

	mu    sync.Mutex
	day   string
	usage map[string]Usage
}

// New returns a Tracker enforcing limits. A zero Limits disables enforcement,
// but usage is still accumulated so it can be reported.
func New(limits Limits) *Tracker {
	return &Tracker{
		limits: limits,
		now:    time.Now,
		usage:  map[string]Usage{},
	}
}

// Enabled reports whether any ceiling is configured.
func (t *Tracker) Enabled() bool {
	return t != nil && (t.limits.TokensPerDay > 0 || t.limits.CostPerDay > 0)
}

// Limits returns the configured ceilings.
func (t *Tracker) Limits() Limits {
	if t == nil {
		return Limits{}
	}
	return t.limits
}

// Day returns the UTC day the tracker is currently accounting for.
func (t *Tracker) Day() string {
	if t == nil {
		return ""
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.rollover()
	return t.day
}

// Seed installs pre-existing usage for a day, typically read back from SQLite at
// startup. Usage for any day other than the current one is ignored.
func (t *Tracker) Seed(day string, usage map[string]Usage) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.rollover()
	if day != t.day {
		return
	}
	for k, u := range usage {
		t.usage[k] = u
	}
}

// Check reports whether key may make another request, and which ceiling it has
// crossed when it may not.
func (t *Tracker) Check(key string) (bool, Reason) {
	if !t.Enabled() {
		return true, ReasonNone
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.rollover()

	u := t.usage[key]
	if t.limits.TokensPerDay > 0 && u.Tokens >= t.limits.TokensPerDay {
		return false, ReasonTokens
	}
	if t.limits.CostPerDay > 0 && u.Cost >= t.limits.CostPerDay {
		return false, ReasonCost
	}
	return true, ReasonNone
}

// Add records consumption against key. Zero usage is ignored so that error rows
// and cache hits do not create empty entries.
func (t *Tracker) Add(key string, tokensUsed int64, cost float64) {
	if t == nil || key == "" || (tokensUsed == 0 && cost == 0) {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.rollover()

	u := t.usage[key]
	u.Tokens += tokensUsed
	u.Cost += cost
	t.usage[key] = u
}

// ResetAt returns the instant the current window ends (the next UTC midnight).
func (t *Tracker) ResetAt() time.Time {
	if t == nil {
		return time.Time{}
	}
	now := t.now().UTC()
	return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, 1)
}

// RetryAfter returns how long until the current window ends, rounded up to whole
// seconds and never below one second.
func (t *Tracker) RetryAfter() time.Duration {
	d := t.ResetAt().Sub(t.now().UTC())
	if d < time.Second {
		return time.Second
	}
	return d.Round(time.Second)
}

// Snapshot returns the current day and a copy of every key's usage.
func (t *Tracker) Snapshot() (string, map[string]Usage) {
	if t == nil {
		return "", map[string]Usage{}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.rollover()

	out := make(map[string]Usage, len(t.usage))
	for k, u := range t.usage {
		out[k] = u
	}
	return t.day, out
}

// rollover clears the counters when the UTC day has changed. Caller must hold mu.
func (t *Tracker) rollover() {
	day := t.now().UTC().Format(DayFormat)
	if t.day == day {
		return
	}
	t.day = day
	t.usage = map[string]Usage{}
}
