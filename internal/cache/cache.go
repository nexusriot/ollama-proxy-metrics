// Package cache is a small in-memory LRU for non-streaming upstream responses.
//
// Identical requests are common in practice — an embedding pipeline re-indexing
// unchanged documents, a retry loop, a dashboard polling the same completion —
// and every one of them costs a full generation. Caching them turns a repeat
// into a memory read, which is why the proxy exposes a hit counter and marks
// served-from-cache rows: the saving is the point, so it has to be measurable.
//
// Entries expire by age (TTL) and are evicted by total size (LRU), so the cache
// never grows past its configured budget.
package cache

import (
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Entry is a cached upstream response.
type Entry struct {
	Status int
	Header http.Header
	Body   []byte
}

// size estimates the memory an entry occupies, including a rough allowance for
// its headers so that header-heavy responses are not accounted as free.
func (e *Entry) size() int64 {
	n := int64(len(e.Body))
	for k, vals := range e.Header {
		n += int64(len(k))
		for _, v := range vals {
			n += int64(len(v))
		}
	}
	return n
}

// item is one LRU list element.
type item struct {
	key     string
	entry   *Entry
	expires time.Time
	size    int64
}

// Cache is a size-bounded, TTL-expiring LRU cache. The zero value is unusable;
// call New. A nil *Cache is safe to use and caches nothing.
type Cache struct {
	ttl      time.Duration
	maxBytes int64
	now      func() time.Time

	mu    sync.Mutex
	ll    *list.List
	items map[string]*list.Element
	bytes int64
}

// New returns a Cache holding entries for ttl and at most maxBytes of response
// bodies. A ttl <= 0 disables caching entirely.
func New(ttl time.Duration, maxBytes int64) *Cache {
	if ttl <= 0 {
		return nil
	}
	return &Cache{
		ttl:      ttl,
		maxBytes: maxBytes,
		now:      time.Now,
		ll:       list.New(),
		items:    map[string]*list.Element{},
	}
}

// Enabled reports whether the cache stores anything.
func (c *Cache) Enabled() bool { return c != nil }

// Key derives a cache key from the parts that must match for two requests to be
// interchangeable.
func Key(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		_, _ = h.Write([]byte(p))
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Get returns the entry for key if it is present and unexpired.
func (c *Cache) Get(key string) (*Entry, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	el, ok := c.items[key]
	if !ok {
		return nil, false
	}
	it := el.Value.(*item)
	if c.now().After(it.expires) {
		c.removeElement(el)
		return nil, false
	}
	c.ll.MoveToFront(el)
	return it.entry, true
}

// Put stores entry under key, evicting least-recently-used entries until the
// cache fits its byte budget. An entry larger than the whole budget is dropped.
func (c *Cache) Put(key string, entry *Entry) {
	if c == nil || entry == nil {
		return
	}
	stored := &Entry{
		Status: entry.Status,
		Header: entry.Header.Clone(),
		Body:   append([]byte(nil), entry.Body...),
	}
	size := stored.size()
	if c.maxBytes > 0 && size > c.maxBytes {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if el, ok := c.items[key]; ok {
		c.removeElement(el)
	}
	el := c.ll.PushFront(&item{
		key:     key,
		entry:   stored,
		expires: c.now().Add(c.ttl),
		size:    size,
	})
	c.items[key] = el
	c.bytes += size

	for c.maxBytes > 0 && c.bytes > c.maxBytes {
		oldest := c.ll.Back()
		if oldest == nil {
			break
		}
		c.removeElement(oldest)
	}
}

// Len returns how many entries the cache holds.
func (c *Cache) Len() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ll.Len()
}

// Bytes returns the total size of the cached entries.
func (c *Cache) Bytes() int64 {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.bytes
}

// Purge empties the cache.
func (c *Cache) Purge() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ll.Init()
	c.items = map[string]*list.Element{}
	c.bytes = 0
}

// removeElement drops el from both the list and the index. Caller must hold mu.
func (c *Cache) removeElement(el *list.Element) {
	it := el.Value.(*item)
	c.ll.Remove(el)
	delete(c.items, it.key)
	c.bytes -= it.size
}

// Cacheable reports whether a response with this status and content type is
// worth storing: only successful JSON documents, never errors or streams.
func Cacheable(status int, contentType string) bool {
	if status < 200 || status >= 300 {
		return false
	}
	ct := strings.ToLower(contentType)
	if ct == "" {
		return true
	}
	return strings.Contains(ct, "json")
}
