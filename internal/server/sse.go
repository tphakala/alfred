// Package server implements the Alfred HTTP server, REST API, and SSE broker.
package server

import (
	"errors"
	"log/slog"
	"sync"
)

const sseChannelBuffer = 64

// ErrBrokerShutdown is returned by Subscribe when the broker has been shut down.
var ErrBrokerShutdown = errors.New("sse broker is shut down")

// SSEEvent represents a server-sent event.
type SSEEvent struct {
	Type string `json:"type"`
	Data string `json:"data"`
}

// SSEBroker manages SSE client subscriptions and broadcasts events.
type SSEBroker struct {
	mu          sync.RWMutex
	subscribers map[chan SSEEvent]struct{}
	logger      *slog.Logger
	closed      bool
}

// NewSSEBroker creates a new SSE broker. If logger is nil, slog.Default() is used.
func NewSSEBroker(logger *slog.Logger) *SSEBroker {
	if logger == nil {
		logger = slog.Default()
	}
	return &SSEBroker{
		subscribers: make(map[chan SSEEvent]struct{}),
		logger:      logger,
	}
}

// Subscribe registers a new client and returns a channel for receiving events.
// Returns ErrBrokerShutdown if the broker has been shut down.
func (b *SSEBroker) Subscribe() (chan SSEEvent, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil, ErrBrokerShutdown
	}
	ch := make(chan SSEEvent, sseChannelBuffer)
	b.subscribers[ch] = struct{}{}
	return ch, nil
}

// Unsubscribe removes a client and closes its channel.
func (b *SSEBroker) Unsubscribe(ch chan SSEEvent) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.subscribers[ch]; !ok {
		return
	}
	delete(b.subscribers, ch)
	close(ch)
}

// Broadcast sends an event to all subscribers. Slow subscribers that have a
// full buffer are skipped (non-blocking send) and a warning is logged.
func (b *SSEBroker) Broadcast(event SSEEvent) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for ch := range b.subscribers {
		select {
		case ch <- event:
		default:
			b.logger.Warn("dropped SSE event for slow subscriber",
				"event_type", event.Type)
		}
	}
}

// Shutdown closes all subscriber channels and prevents new subscriptions.
func (b *SSEBroker) Shutdown() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	for ch := range b.subscribers {
		close(ch)
	}
	clear(b.subscribers)
}

// SessionEventBroker manages session-scoped SSE subscriptions.
// Activities push events keyed by session ID; only subscribers for that session receive them.
type SessionEventBroker struct {
	mu       sync.RWMutex
	channels map[string]map[chan SSEEvent]struct{} // sessionID -> subscriber channels
	logger   *slog.Logger
}

// NewSessionEventBroker creates a new SessionEventBroker. If logger is nil,
// slog.Default() is used.
func NewSessionEventBroker(logger *slog.Logger) *SessionEventBroker {
	if logger == nil {
		logger = slog.Default()
	}
	return &SessionEventBroker{
		channels: make(map[string]map[chan SSEEvent]struct{}),
		logger:   logger,
	}
}

// Subscribe creates a channel that receives events for the given session ID.
func (b *SessionEventBroker) Subscribe(sessionID string) chan SSEEvent {
	b.mu.Lock() // write lock: modifying the map
	defer b.mu.Unlock()
	ch := make(chan SSEEvent, sseChannelBuffer)
	if b.channels[sessionID] == nil {
		b.channels[sessionID] = make(map[chan SSEEvent]struct{})
	}
	b.channels[sessionID][ch] = struct{}{}
	return ch
}

// Unsubscribe removes a subscriber channel for a session and closes it.
// Safe to call multiple times; a channel that has already been unsubscribed
// (or was never subscribed) is silently ignored.
func (b *SessionEventBroker) Unsubscribe(sessionID string, ch chan SSEEvent) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if subs, ok := b.channels[sessionID]; ok {
		if _, exists := subs[ch]; exists {
			delete(subs, ch)
			if len(subs) == 0 {
				delete(b.channels, sessionID)
			}
			close(ch)
		}
	}
}

// Publish sends an event to all subscribers of a specific session.
// Slow subscribers are skipped (non-blocking send) and a warning is logged.
func (b *SessionEventBroker) Publish(sessionID string, event SSEEvent) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for ch := range b.channels[sessionID] {
		select {
		case ch <- event:
		default:
			b.logger.Warn("dropped session SSE event for slow subscriber",
				"session_id", sessionID,
				"event_type", event.Type)
		}
	}
}

// Shutdown closes all subscriber channels for all sessions.
func (b *SessionEventBroker) Shutdown() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, subs := range b.channels {
		for ch := range subs {
			close(ch)
		}
	}
	clear(b.channels)
}

// SessionBrokerAdapter wraps SessionEventBroker to satisfy
// workflow.SessionEventPublisher, breaking the import cycle between
// the server and workflow packages.
type SessionBrokerAdapter struct {
	Broker *SessionEventBroker
}

// Publish implements workflow.SessionEventPublisher using primitive parameters.
func (a *SessionBrokerAdapter) Publish(sessionID, eventType, data string) {
	a.Broker.Publish(sessionID, SSEEvent{Type: eventType, Data: data})
}
