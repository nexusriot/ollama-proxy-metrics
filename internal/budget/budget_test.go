package budget

import (
	"testing"
	"time"
)

// at pins the tracker's clock so day rollover is testable.
func at(t *testing.T, tr *Tracker, ts string) {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		t.Fatalf("parse %q: %v", ts, err)
	}
	tr.now = func() time.Time { return parsed }
}

func TestTracker_DisabledAlwaysAllows(t *testing.T) {
	tr := New(Limits{})
	if tr.Enabled() {
		t.Fatal("Enabled() = true for zero limits")
	}
	tr.Add("s", 1_000_000, 999)
	if ok, reason := tr.Check("s"); !ok || reason != ReasonNone {
		t.Fatalf("Check on disabled tracker = %v, %q", ok, reason)
	}
}

func TestTracker_NilIsSafe(t *testing.T) {
	var tr *Tracker
	if tr.Enabled() {
		t.Fatal("nil tracker Enabled() = true")
	}
	tr.Add("s", 5, 5)
	if ok, _ := tr.Check("s"); !ok {
		t.Fatal("nil tracker refused a request")
	}
	if day, usage := tr.Snapshot(); day != "" || len(usage) != 0 {
		t.Fatal("nil tracker Snapshot should be empty")
	}
}

func TestTracker_TokenCeiling(t *testing.T) {
	tr := New(Limits{TokensPerDay: 100})
	at(t, tr, "2026-08-28T10:00:00Z")

	tr.Add("sess", 99, 0)
	if ok, _ := tr.Check("sess"); !ok {
		t.Fatal("Check refused a session under its token ceiling")
	}
	// The request that crosses the line is allowed; the next one is refused.
	tr.Add("sess", 10, 0)
	ok, reason := tr.Check("sess")
	if ok || reason != ReasonTokens {
		t.Fatalf("Check over token ceiling = %v, %q; want false, tokens_per_day", ok, reason)
	}
	if ok, _ := tr.Check("other"); !ok {
		t.Fatal("one session's overage must not affect another")
	}
}

func TestTracker_CostCeiling(t *testing.T) {
	tr := New(Limits{CostPerDay: 0.50})
	at(t, tr, "2026-08-28T10:00:00Z")

	tr.Add("sess", 10, 0.49)
	if ok, _ := tr.Check("sess"); !ok {
		t.Fatal("Check refused a session under its cost ceiling")
	}
	tr.Add("sess", 10, 0.02)
	ok, reason := tr.Check("sess")
	if ok || reason != ReasonCost {
		t.Fatalf("Check over cost ceiling = %v, %q; want false, cost_per_day", ok, reason)
	}
}

func TestTracker_RollsOverAtUTCMidnight(t *testing.T) {
	tr := New(Limits{TokensPerDay: 10})
	at(t, tr, "2026-08-28T23:59:00Z")
	tr.Add("sess", 50, 0)
	if ok, _ := tr.Check("sess"); ok {
		t.Fatal("Check should refuse before the window ends")
	}

	at(t, tr, "2026-08-29T00:00:01Z")
	if ok, _ := tr.Check("sess"); !ok {
		t.Fatal("Check should allow again in the next day's window")
	}
	if day, usage := tr.Snapshot(); day != "2026-08-29" || len(usage) != 0 {
		t.Fatalf("Snapshot after rollover = %q, %v", day, usage)
	}
}

func TestTracker_SeedOnlyAcceptsCurrentDay(t *testing.T) {
	tr := New(Limits{TokensPerDay: 100})
	at(t, tr, "2026-08-28T10:00:00Z")

	tr.Seed("2026-08-27", map[string]Usage{"sess": {Tokens: 500}})
	if ok, _ := tr.Check("sess"); !ok {
		t.Fatal("stale seed must be ignored")
	}

	tr.Seed("2026-08-28", map[string]Usage{"sess": {Tokens: 500, Cost: 1}})
	if ok, reason := tr.Check("sess"); ok || reason != ReasonTokens {
		t.Fatalf("Check after seeding today = %v, %q", ok, reason)
	}
	_, usage := tr.Snapshot()
	if usage["sess"].Tokens != 500 {
		t.Fatalf("seeded usage = %+v", usage["sess"])
	}
}

func TestTracker_AddIgnoresEmptyUsage(t *testing.T) {
	tr := New(Limits{TokensPerDay: 100})
	at(t, tr, "2026-08-28T10:00:00Z")
	tr.Add("sess", 0, 0)
	tr.Add("", 10, 1)
	if _, usage := tr.Snapshot(); len(usage) != 0 {
		t.Fatalf("Snapshot = %v, want no entries", usage)
	}
}

func TestTracker_RetryAfterUntilMidnight(t *testing.T) {
	tr := New(Limits{TokensPerDay: 1})
	at(t, tr, "2026-08-28T23:30:00Z")
	if got := tr.RetryAfter(); got != 30*time.Minute {
		t.Fatalf("RetryAfter = %v, want 30m", got)
	}
	if got := tr.ResetAt().Format(time.RFC3339); got != "2026-08-29T00:00:00Z" {
		t.Fatalf("ResetAt = %s", got)
	}
}
