package cache

import (
	"net/http"
	"testing"
	"time"
)

func newEntry(body string) *Entry {
	return &Entry{
		Status: http.StatusOK,
		Header: http.Header{"Content-Type": []string{"application/json"}},
		Body:   []byte(body),
	}
}

func TestCache_DisabledWhenTTLZero(t *testing.T) {
	c := New(0, 1<<20)
	if c.Enabled() {
		t.Fatal("New(0, …).Enabled() = true, want false")
	}
	c.Put("k", newEntry("x"))
	if _, ok := c.Get("k"); ok {
		t.Fatal("disabled cache returned a hit")
	}
	if c.Len() != 0 || c.Bytes() != 0 {
		t.Fatal("disabled cache should report empty")
	}
}

func TestCache_PutGet(t *testing.T) {
	c := New(time.Minute, 1<<20)
	c.Put("k", newEntry(`{"a":1}`))

	got, ok := c.Get("k")
	if !ok {
		t.Fatal("Get after Put = miss")
	}
	if string(got.Body) != `{"a":1}` || got.Status != http.StatusOK {
		t.Fatalf("cached entry = %d %q", got.Status, got.Body)
	}
	if got.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("cached header = %v", got.Header)
	}
}

func TestCache_StoresACopy(t *testing.T) {
	c := New(time.Minute, 1<<20)
	e := newEntry("original")
	c.Put("k", e)

	e.Body[0] = 'X'
	e.Header.Set("Content-Type", "text/plain")

	got, _ := c.Get("k")
	if string(got.Body) != "original" {
		t.Fatalf("cached body mutated by the caller: %q", got.Body)
	}
	if got.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("cached header mutated by the caller: %v", got.Header)
	}
}

func TestCache_Expires(t *testing.T) {
	c := New(time.Minute, 1<<20)
	now := time.Unix(1_700_000_000, 0)
	c.now = func() time.Time { return now }

	c.Put("k", newEntry("v"))
	if _, ok := c.Get("k"); !ok {
		t.Fatal("fresh entry missing")
	}

	now = now.Add(2 * time.Minute)
	if _, ok := c.Get("k"); ok {
		t.Fatal("expired entry returned a hit")
	}
	if c.Len() != 0 {
		t.Fatalf("expired entry not dropped: Len = %d", c.Len())
	}
}

func TestCache_EvictsLeastRecentlyUsed(t *testing.T) {
	// Budget fits two 10-byte bodies plus their identical headers.
	one := newEntry("0123456789")
	c := New(time.Minute, 2*one.size())

	c.Put("a", newEntry("0123456789"))
	c.Put("b", newEntry("0123456789"))
	if _, ok := c.Get("a"); !ok { // touch "a" so "b" becomes the oldest
		t.Fatal("entry a missing")
	}
	c.Put("c", newEntry("0123456789"))

	if _, ok := c.Get("b"); ok {
		t.Fatal("least-recently-used entry b survived eviction")
	}
	if _, ok := c.Get("a"); !ok {
		t.Fatal("recently used entry a was evicted")
	}
	if _, ok := c.Get("c"); !ok {
		t.Fatal("newest entry c missing")
	}
	if c.Bytes() > 2*one.size() {
		t.Fatalf("cache over budget: %d bytes", c.Bytes())
	}
}

func TestCache_RejectsOversizedEntry(t *testing.T) {
	c := New(time.Minute, 8)
	c.Put("k", newEntry("far too large for the budget"))
	if _, ok := c.Get("k"); ok {
		t.Fatal("oversized entry was stored")
	}
	if c.Bytes() != 0 {
		t.Fatalf("Bytes = %d after rejecting an oversized entry", c.Bytes())
	}
}

func TestCache_ReplaceKeepsAccountingStraight(t *testing.T) {
	c := New(time.Minute, 1<<20)
	c.Put("k", newEntry("first"))
	c.Put("k", newEntry("second"))

	if c.Len() != 1 {
		t.Fatalf("Len = %d after replacing a key, want 1", c.Len())
	}
	got, _ := c.Get("k")
	if string(got.Body) != "second" {
		t.Fatalf("entry = %q, want the replacement", got.Body)
	}
	if want := got.size(); c.Bytes() != want {
		t.Fatalf("Bytes = %d, want %d", c.Bytes(), want)
	}
}

func TestCache_Purge(t *testing.T) {
	c := New(time.Minute, 1<<20)
	c.Put("a", newEntry("x"))
	c.Put("b", newEntry("y"))
	c.Purge()
	if c.Len() != 0 || c.Bytes() != 0 {
		t.Fatalf("after Purge: Len = %d, Bytes = %d", c.Len(), c.Bytes())
	}
	if _, ok := c.Get("a"); ok {
		t.Fatal("Get after Purge = hit")
	}
}

func TestKey_DistinguishesPartBoundaries(t *testing.T) {
	if Key("ab", "c") == Key("a", "bc") {
		t.Fatal("Key must not collide across part boundaries")
	}
	if Key("a", "b") != Key("a", "b") {
		t.Fatal("Key must be stable for equal input")
	}
}

func TestCacheable(t *testing.T) {
	cases := []struct {
		status int
		ct     string
		want   bool
	}{
		{200, "application/json", true},
		{200, "", true},
		{200, "text/event-stream", false},
		{201, "application/json; charset=utf-8", true},
		{404, "application/json", false},
		{500, "application/json", false},
	}
	for _, c := range cases {
		if got := Cacheable(c.status, c.ct); got != c.want {
			t.Errorf("Cacheable(%d, %q) = %v, want %v", c.status, c.ct, got, c.want)
		}
	}
}
