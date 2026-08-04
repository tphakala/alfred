package server_test

import (
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/tphakala/alfred/internal/server"
)

const (
	testSSEEventType = "test"
	testSSEDataHello = "hello"
)

func newTestBroker(t *testing.T) *server.SSEBroker {
	t.Helper()
	return server.NewSSEBroker(slog.Default())
}

func TestSSEBroker_SubscribeAndBroadcast(t *testing.T) {
	broker := newTestBroker(t)

	ch, err := broker.Subscribe()
	if err != nil {
		t.Fatalf("Subscribe() error: %v", err)
	}
	defer broker.Unsubscribe(ch)

	broker.Broadcast(server.SSEEvent{Type: testSSEEventType, Data: testSSEDataHello})

	select {
	case event := <-ch:
		if event.Type != testSSEEventType {
			t.Errorf("event.Type = %q, want %q", event.Type, testSSEEventType)
		}
		if event.Data != "hello" {
			t.Errorf("event.Data = %q, want %q", event.Data, "hello")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for event")
	}
}

func TestSSEBroker_UnsubscribeStopsEvents(t *testing.T) {
	broker := newTestBroker(t)

	ch, err := broker.Subscribe()
	if err != nil {
		t.Fatalf("Subscribe() error: %v", err)
	}
	broker.Unsubscribe(ch)

	broker.Broadcast(server.SSEEvent{Type: testSSEEventType, Data: "after-unsub"})

	select {
	case event, ok := <-ch:
		if ok {
			t.Errorf("received event after unsubscribe: %+v", event)
		}
	case <-time.After(100 * time.Millisecond):
		// expected — no event received
	}
}

func TestSSEBroker_MultipleSubscribers(t *testing.T) {
	broker := newTestBroker(t)

	ch1, err := broker.Subscribe()
	if err != nil {
		t.Fatalf("Subscribe() ch1 error: %v", err)
	}
	defer broker.Unsubscribe(ch1)
	ch2, err := broker.Subscribe()
	if err != nil {
		t.Fatalf("Subscribe() ch2 error: %v", err)
	}
	defer broker.Unsubscribe(ch2)

	broker.Broadcast(server.SSEEvent{Type: "multi", Data: "all"})

	for i, ch := range []chan server.SSEEvent{ch1, ch2} {
		select {
		case event := <-ch:
			if event.Type != "multi" || event.Data != "all" {
				t.Errorf("subscriber %d: unexpected event: %+v", i, event)
			}
		case <-time.After(time.Second):
			t.Fatalf("subscriber %d: timed out waiting for event", i)
		}
	}
}

func TestSSEBroker_ShutdownRejectsSubscribe(t *testing.T) {
	broker := newTestBroker(t)
	broker.Shutdown()

	_, err := broker.Subscribe()
	if !errors.Is(err, server.ErrBrokerShutdown) {
		t.Errorf("Subscribe() after Shutdown: got err=%v, want %v", err, server.ErrBrokerShutdown)
	}
}

func TestSSEBroker_ShutdownClosesExistingChannels(t *testing.T) {
	broker := newTestBroker(t)

	ch, err := broker.Subscribe()
	if err != nil {
		t.Fatalf("Subscribe() error: %v", err)
	}

	broker.Shutdown()

	select {
	case _, ok := <-ch:
		if ok {
			t.Error("expected channel to be closed after Shutdown")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for channel close after Shutdown")
	}
}

func TestSSEBroker_UnsubscribeAfterShutdown(t *testing.T) {
	broker := newTestBroker(t)

	ch, err := broker.Subscribe()
	if err != nil {
		t.Fatalf("Subscribe() error: %v", err)
	}

	broker.Shutdown()
	broker.Unsubscribe(ch) // must not panic (double-close guard)
}

func TestSSEBroker_BroadcastAfterShutdown(t *testing.T) {
	broker := newTestBroker(t)
	broker.Shutdown()
	broker.Broadcast(server.SSEEvent{Type: "test", Data: "ignored"}) // must not panic
}

func TestSessionEventBroker_PublishToCorrectSession(t *testing.T) {
	b := server.NewSessionEventBroker(nil)
	ch1 := b.Subscribe("session-1")
	ch2 := b.Subscribe("session-2")

	b.Publish("session-1", server.SSEEvent{Type: "chunk", Data: testSSEDataHello})

	select {
	case evt := <-ch1:
		if evt.Data != "hello" {
			t.Errorf("data = %q", evt.Data)
		}
	default:
		t.Error("session-1 subscriber did not receive event")
	}

	select {
	case <-ch2:
		t.Error("session-2 subscriber should not receive session-1 event")
	default:
		// expected
	}

	b.Unsubscribe("session-1", ch1)
	b.Unsubscribe("session-2", ch2)
}

func TestSessionEventBroker_MultipleSubscribers(t *testing.T) {
	b := server.NewSessionEventBroker(nil)
	ch1 := b.Subscribe("session-1")
	ch2 := b.Subscribe("session-1")

	b.Publish("session-1", server.SSEEvent{Type: "chunk", Data: "broadcast"})

	for _, ch := range []chan server.SSEEvent{ch1, ch2} {
		select {
		case evt := <-ch:
			if evt.Data != "broadcast" {
				t.Errorf("data = %q", evt.Data)
			}
		default:
			t.Error("subscriber did not receive event")
		}
	}

	b.Unsubscribe("session-1", ch1)
	b.Unsubscribe("session-1", ch2)
}

func TestSessionEventBroker_UnsubscribeCleansUp(t *testing.T) {
	b := server.NewSessionEventBroker(nil)
	ch := b.Subscribe("session-1")
	b.Unsubscribe("session-1", ch)

	// Publishing to empty session should not panic
	b.Publish("session-1", server.SSEEvent{Type: "test", Data: "data"})
}
