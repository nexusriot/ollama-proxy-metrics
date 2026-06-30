package events

import (
	"testing"
	"time"
)

func TestBroker_PublishToSubscribers(t *testing.T) {
	b := NewBroker()
	ch, cancel := b.Subscribe()
	defer cancel()

	if b.Subscribers() != 1 {
		t.Fatalf("expected 1 subscriber, got %d", b.Subscribers())
	}

	b.Publish([]byte("hello"))
	select {
	case msg := <-ch:
		if string(msg) != "hello" {
			t.Errorf("got %q, want hello", msg)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for published message")
	}
}

func TestBroker_UnsubscribeStopsDelivery(t *testing.T) {
	b := NewBroker()
	_, cancel := b.Subscribe()
	cancel()
	if b.Subscribers() != 0 {
		t.Errorf("expected 0 subscribers after cancel, got %d", b.Subscribers())
	}
	// Publishing with no subscribers must not panic or block.
	b.Publish([]byte("x"))
}

func TestBroker_SlowSubscriberDoesNotBlock(t *testing.T) {
	b := NewBroker()
	_, cancel := b.Subscribe()
	defer cancel()
	// Far exceed the channel buffer; Publish must drop, never block.
	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			b.Publish([]byte("flood"))
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Publish blocked on a slow subscriber")
	}
}
