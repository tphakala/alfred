package workflow

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/tphakala/alfred/internal/agent/runner"
	"github.com/tphakala/alfred/internal/ctxbuild"
	"github.com/tphakala/alfred/internal/store"
)

// StoreEventSink persists each runner.Event as a row in the messages table
// and broadcasts it on the SSE channel. It implements the EventSink interface.
type StoreEventSink struct {
	store  *store.Store
	broker SessionEventPublisher
}

// NewStoreEventSink constructs an EventSink backed by the Alfred store and
// SSE broadcaster. The broadcaster can be nil for headless runs.
func NewStoreEventSink(s *store.Store, b SessionEventPublisher) *StoreEventSink {
	return &StoreEventSink{store: s, broker: b}
}

// Persist writes one messages row for the given event. Sequence numbers are
// assigned automatically by AppendMessagesAutoSeq.
func (s *StoreEventSink) Persist(ctx context.Context, sessionID uuid.UUID, phase string, ev runner.Event) error { //nolint:gocritic // hugeParam: runner.Event is value-semantic on the event path
	role, content, meta := MapEventToMessage(phase, ev)
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("marshal metadata: %w", err)
	}
	msg := store.Message{
		ID:        uuid.New(),
		SessionID: sessionID,
		Role:      role,
		Content:   content,
		Metadata:  metaJSON,
	}
	return s.store.AppendMessagesAutoSeq(ctx, sessionID, []store.Message{msg})
}

// Broadcast forwards the event to SSE subscribers for the given session.
func (s *StoreEventSink) Broadcast(sessionID uuid.UUID, ev runner.Event) { //nolint:gocritic // hugeParam: runner.Event is value-semantic on the event path
	if s.broker == nil {
		return
	}
	data, err := json.Marshal(ev)
	if err != nil {
		return
	}
	eventType := "agent_event"
	if ev.Kind == runner.KindReplay {
		eventType = "guidance_received"
	}
	s.broker.Publish(sessionID.String(), eventType, string(data))
}

// MapEventToMessage converts a runner.Event to the corresponding messages table
// role, content, and metadata. Exported for testing.
func MapEventToMessage(phase string, ev runner.Event) (role, content string, meta map[string]any) { //nolint:gocritic // hugeParam: runner.Event is value-semantic on the event path
	meta = map[string]any{"phase": phase, "event": ev.Kind.String()}
	switch ev.Kind {
	case runner.KindSessionStart:
		role, content = ctxbuild.RoleContext, ""
	case runner.KindTextDelta:
		role, content = ctxbuild.RoleModel, ev.Text
	case runner.KindToolCall:
		role = ctxbuild.RoleToolCall
		content = ""
		meta["call_id"] = ev.CallID
		meta["tool"] = ev.Tool
		if len(ev.Args) > 0 {
			meta["args"] = ev.Args
		}
	case runner.KindToolResult:
		role = ctxbuild.RoleToolResult
		content = string(ev.Result)
		meta["call_id"] = ev.CallID
		meta["tool"] = ev.Tool
	case runner.KindApiRetry:
		role, content = ctxbuild.RoleModel, ""
		meta["text"] = ev.Text
	case runner.KindError:
		role, content = ctxbuild.RoleError, ev.Text
	case runner.KindStructuredOutput:
		role, content = ctxbuild.RoleModel, string(ev.Result)
	case runner.KindDone:
		role, content = ctxbuild.RoleContext, ""
		meta["usage"] = ev.UsageDelta
	case runner.KindReplay:
		role = ctxbuild.RoleUser
		content = ev.Text
	default:
		role, content = ctxbuild.RoleContext, ""
	}
	return role, content, meta
}
