// Package events provides a tiny in-process fan-out broker used to push newly
// recorded requests to live dashboard subscribers (Server-Sent Events).
//
// Publish never blocks: each subscriber has a small buffered channel and a
// message is dropped for any subscriber that cannot keep up. The live tail is a
// best-effort convenience, not a guaranteed delivery stream — SQLite remains the
// source of truth.
package events

import "sync"

// Broker fans messages out to all current subscribers.
type Broker struct {
	mu   sync.RWMutex
	subs map[chan []byte]struct{}
}

// NewBroker creates an empty Broker.
func NewBroker() *Broker {
	return &Broker{subs: make(map[chan []byte]struct{})}
}

// Subscribe registers a new subscriber and returns its channel plus an
// unsubscribe function that must be called when the subscriber goes away.
func (b *Broker) Subscribe() (<-chan []byte, func()) {
	ch := make(chan []byte, 16)
	b.mu.Lock()
	b.subs[ch] = struct{}{}
	b.mu.Unlock()

	var once sync.Once
	cancel := func() {
		once.Do(func() {
			b.mu.Lock()
			delete(b.subs, ch)
			b.mu.Unlock()
			close(ch)
		})
	}
	return ch, cancel
}

// Publish delivers msg to every subscriber, dropping it for any whose buffer is
// full. It is safe to call from many goroutines and never blocks.
func (b *Broker) Publish(msg []byte) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for ch := range b.subs {
		select {
		case ch <- msg:
		default: // subscriber is slow; drop this message for them
		}
	}
}

// Subscribers returns the current subscriber count (used by tests/metrics).
func (b *Broker) Subscribers() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.subs)
}
