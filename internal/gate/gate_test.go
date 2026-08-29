package gate

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestGate_DisabledAlwaysAdmits(t *testing.T) {
	g := New(0, 0)
	if g.Enabled() {
		t.Fatal("New(0,0).Enabled() = true, want false")
	}
	for i := 0; i < 100; i++ {
		release, err := g.Acquire(context.Background())
		if err != nil {
			t.Fatalf("Acquire on disabled gate: %v", err)
		}
		release()
	}
}

func TestGate_NilIsSafe(t *testing.T) {
	var g *Gate
	if g.Enabled() || g.Inflight() != 0 || g.Waiting() != 0 {
		t.Fatal("nil gate should report disabled and empty")
	}
}

func TestGate_LimitsConcurrency(t *testing.T) {
	g := New(1, 0)
	first, err := g.Acquire(context.Background())
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	if got := g.Inflight(); got != 1 {
		t.Fatalf("Inflight = %d, want 1", got)
	}

	admitted := make(chan struct{})
	go func() {
		release, err := g.Acquire(context.Background())
		if err == nil {
			release()
		}
		close(admitted)
	}()

	select {
	case <-admitted:
		t.Fatal("second Acquire was admitted while the only slot was held")
	case <-time.After(50 * time.Millisecond):
	}

	first()
	select {
	case <-admitted:
	case <-time.After(time.Second):
		t.Fatal("second Acquire never admitted after release")
	}
}

func TestGate_QueueFull(t *testing.T) {
	g := New(1, 1)
	held, err := g.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	queued := make(chan struct{})
	go func() {
		release, err := g.Acquire(context.Background())
		if err == nil {
			release()
		}
		close(queued)
	}()

	// Wait for the queued caller to register before testing the overflow.
	deadline := time.Now().Add(time.Second)
	for g.Waiting() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if g.Waiting() != 1 {
		t.Fatalf("Waiting = %d, want 1", g.Waiting())
	}

	if _, err := g.Acquire(context.Background()); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("Acquire over queue limit = %v, want ErrQueueFull", err)
	}
	held()
	<-queued
}

func TestGate_ContextCancelWhileWaiting(t *testing.T) {
	g := New(1, 0)
	held, err := g.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer held()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := g.Acquire(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Acquire with cancelled context = %v, want context.Canceled", err)
	}
}

func TestGate_DoubleReleaseIsIgnored(t *testing.T) {
	g := New(1, 0)
	release, err := g.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	release()
	release()

	// The slot must still be available to the next caller.
	next, err := g.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire after double release: %v", err)
	}
	next()
	if got := g.Inflight(); got != 0 {
		t.Fatalf("Inflight = %d, want 0", got)
	}
}

func TestGate_ReleasesUnderConcurrency(t *testing.T) {
	g := New(4, 0)
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release, err := g.Acquire(context.Background())
			if err != nil {
				t.Errorf("Acquire: %v", err)
				return
			}
			release()
		}()
	}
	wg.Wait()
	if got := g.Inflight(); got != 0 {
		t.Fatalf("Inflight after all releases = %d, want 0", got)
	}
}
