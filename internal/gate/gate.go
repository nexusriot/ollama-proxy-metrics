// Package gate caps how many generation requests are in flight at once.
//
// Ollama serializes generation internally, so a burst of parallel clients does
// not go faster — it thrashes VRAM and inflates every request's latency. A
// semaphore in front of the upstream turns that thrash into an orderly queue,
// and a bounded queue means an overloaded proxy rejects fast instead of piling
// up goroutines that will time out anyway.
package gate

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
)

// ErrQueueFull is returned by Acquire when the wait queue is at its limit.
var ErrQueueFull = errors.New("gate: queue full")

// Gate admits at most maxConcurrent holders at a time, queueing up to maxQueue
// further callers. A maxConcurrent of 0 disables the gate entirely.
type Gate struct {
	slots    chan struct{}
	maxQueue int

	waiting  atomic.Int64
	inflight atomic.Int64
}

// New returns a Gate admitting maxConcurrent concurrent holders with at most
// maxQueue waiters. maxConcurrent <= 0 disables gating; maxQueue <= 0 leaves the
// queue unbounded.
func New(maxConcurrent, maxQueue int) *Gate {
	if maxConcurrent <= 0 {
		return &Gate{}
	}
	return &Gate{
		slots:    make(chan struct{}, maxConcurrent),
		maxQueue: maxQueue,
	}
}

// Enabled reports whether the gate limits anything.
func (g *Gate) Enabled() bool { return g != nil && g.slots != nil }

// Acquire takes a slot, blocking until one frees up. It returns a release
// function that must be called when the work is done. It fails immediately with
// ErrQueueFull when the queue is full, and with ctx.Err() if the caller goes
// away while waiting.
func (g *Gate) Acquire(ctx context.Context) (release func(), err error) {
	if !g.Enabled() {
		return func() {}, nil
	}
	select {
	case g.slots <- struct{}{}:
		return g.acquired(), nil
	default:
	}

	if g.maxQueue > 0 && g.waiting.Load() >= int64(g.maxQueue) {
		return nil, ErrQueueFull
	}

	g.waiting.Add(1)
	defer g.waiting.Add(-1)
	select {
	case g.slots <- struct{}{}:
		return g.acquired(), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// acquired books a taken slot and returns its release function. Release is
// guarded by a sync.Once: a caller that releases twice would otherwise drain a
// slot it never held and permanently shrink the gate.
func (g *Gate) acquired() func() {
	g.inflight.Add(1)
	var once sync.Once
	return func() {
		once.Do(func() {
			g.inflight.Add(-1)
			<-g.slots
		})
	}
}

// Inflight returns how many holders currently occupy a slot.
func (g *Gate) Inflight() int64 {
	if g == nil {
		return 0
	}
	return g.inflight.Load()
}

// Waiting returns how many callers are currently queued.
func (g *Gate) Waiting() int64 {
	if g == nil {
		return 0
	}
	return g.waiting.Load()
}
