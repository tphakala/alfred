package server

import "sync"

// guidanceChannelBuffer bounds the number of unconsumed guidance messages per
// run. Reaching the limit means the activity-side reader is stuck; further
// pushes return false rather than blocking the workflow's update handler.
const guidanceChannelBuffer = 16

// GuidanceStore is an in-process registry of per-run guidance channels. The
// workflow's inject_guidance update handler pushes into the channel; the
// activity that owns the subprocess reads from it and writes user-message
// NDJSON into the runner's stdin pipe. State dies with the Alfred process,
// matching the PermitStore lifecycle.
type GuidanceStore struct {
	mu    sync.Mutex
	chans map[string]chan string
}

// NewGuidanceStore creates a new GuidanceStore ready for use.
func NewGuidanceStore() *GuidanceStore {
	return &GuidanceStore{chans: make(map[string]chan string)}
}

// RegisterRun creates a new guidance channel for runID. Idempotent: registering
// the same runID twice without an intervening DeregisterRun replaces the prior
// channel and discards any buffered messages.
func (g *GuidanceStore) RegisterRun(runID string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.chans[runID] = make(chan string, guidanceChannelBuffer)
}

// DeregisterRun closes the channel for runID. Safe to call when no channel is
// registered.
func (g *GuidanceStore) DeregisterRun(runID string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if ch, ok := g.chans[runID]; ok {
		close(ch)
		delete(g.chans, runID)
	}
}

// Receiver returns the read-only channel for runID and a presence boolean.
// Returns (nil, false) if the run is not registered.
func (g *GuidanceStore) Receiver(runID string) (<-chan string, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	ch, ok := g.chans[runID]
	if !ok {
		return nil, false
	}
	return ch, true
}

// Push enqueues guidance for the run. Returns false if the run is not
// registered or the channel buffer is full (the workflow update handler
// surfaces a typed error in that case so the UI can warn the operator).
//
// The mutex is held through the send to make the validity check and the
// channel send atomic with respect to DeregisterRun, which closes the
// channel. Without this, a concurrent DeregisterRun between the lookup and
// the send would panic on send-to-closed-channel. The select has a default
// case, so the lock is held for a bounded time and cannot deadlock.
func (g *GuidanceStore) Push(runID, text string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	ch, ok := g.chans[runID]
	if !ok {
		return false
	}
	select {
	case ch <- text:
		return true
	default:
		return false
	}
}
