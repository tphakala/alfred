package workflow

import (
	"encoding/json"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/tphakala/alfred/internal/agent/runner"
	"github.com/tphakala/alfred/internal/ctxbuild"
)

func TestMapEventToMessage_KindToRole(t *testing.T) {
	cases := []struct {
		ev   runner.Event
		role string
	}{
		{runner.Event{Kind: runner.KindSessionStart}, ctxbuild.RoleContext},
		{runner.Event{Kind: runner.KindTextDelta, Text: "hi"}, ctxbuild.RoleModel},
		{runner.Event{Kind: runner.KindToolCall, CallID: "c1", Tool: "Bash"}, ctxbuild.RoleToolCall},
		{runner.Event{Kind: runner.KindToolResult, CallID: "c1", Result: json.RawMessage(`"ok"`)}, ctxbuild.RoleToolResult},
		{runner.Event{Kind: runner.KindApiRetry, Text: "retry"}, ctxbuild.RoleModel},
		{runner.Event{Kind: runner.KindError, Text: "boom"}, ctxbuild.RoleError},
		{runner.Event{Kind: runner.KindStructuredOutput, Result: json.RawMessage(`{"key":"val"}`)}, ctxbuild.RoleModel},
		{runner.Event{Kind: runner.KindDone}, ctxbuild.RoleContext},
	}
	for _, c := range cases {
		role, _, _ := MapEventToMessage("main", c.ev)
		assert.Equal(t, c.role, role, "kind=%s", c.ev.Kind.String())
	}
}

func TestMapEventToMessage_TextDeltaContent(t *testing.T) {
	_, content, _ := MapEventToMessage("main", runner.Event{Kind: runner.KindTextDelta, Text: "hello"})
	assert.Equal(t, "hello", content)
}

func TestMapEventToMessage_ErrorContent(t *testing.T) {
	_, content, _ := MapEventToMessage("main", runner.Event{Kind: runner.KindError, Text: "something broke"})
	assert.Equal(t, "something broke", content)
}

func TestMapEventToMessage_ToolCallMetadata(t *testing.T) {
	_, _, meta := MapEventToMessage("main", runner.Event{
		Kind:   runner.KindToolCall,
		CallID: "abc",
		Tool:   "Bash",
		Args:   json.RawMessage(`{"command":"ls"}`),
	})
	assert.Equal(t, "abc", meta["call_id"])
	assert.Equal(t, "Bash", meta["tool"])
}

func TestMapEventToMessage_ToolResultMetadata(t *testing.T) {
	_, content, meta := MapEventToMessage("main", runner.Event{
		Kind:   runner.KindToolResult,
		CallID: "xyz",
		Tool:   "Read",
		Result: json.RawMessage(`"file contents"`),
	})
	assert.Equal(t, `"file contents"`, content)
	assert.Equal(t, "xyz", meta["call_id"])
	assert.Equal(t, "Read", meta["tool"])
}

func TestMapEventToMessage_PhaseInMetadata(t *testing.T) {
	_, _, meta := MapEventToMessage("scout", runner.Event{Kind: runner.KindTextDelta, Text: "x"})
	assert.Equal(t, "scout", meta["phase"])
}

func TestMapEventToMessage_EventKindInMetadata(t *testing.T) {
	_, _, meta := MapEventToMessage("main", runner.Event{Kind: runner.KindError, Text: "fail"})
	assert.Equal(t, "error", meta["event"])
}

func TestMapEventToMessage_DoneIncludesUsage(t *testing.T) {
	usage := runner.TokenUsage{InputTokens: 100, OutputTokens: 50, TotalTokens: 150}
	_, _, meta := MapEventToMessage("main", runner.Event{Kind: runner.KindDone, UsageDelta: usage})
	assert.Equal(t, usage, meta["usage"])
}

func TestMapEventToMessage_UnknownKindDefaultsToContext(t *testing.T) {
	role, content, _ := MapEventToMessage("main", runner.Event{Kind: runner.KindUnknown})
	assert.Equal(t, ctxbuild.RoleContext, role)
	assert.Empty(t, content)
}

func TestStoreEventSink_BroadcastReachesSSE(t *testing.T) {
	broker := &fakePublisher{}
	sink := NewStoreEventSink(nil, broker)

	sessID := uuid.New()
	ev := runner.Event{Kind: runner.KindTextDelta, Text: "ping"}
	sink.Broadcast(sessID, ev)
	assert.Equal(t, 1, broker.Count())
}

func TestStoreEventSink_BroadcastNilBrokerNoPanic(t *testing.T) {
	sink := NewStoreEventSink(nil, nil)
	// Must not panic.
	sink.Broadcast(uuid.New(), runner.Event{Kind: runner.KindTextDelta, Text: "x"})
}

// fakePublisher satisfies SessionEventPublisher for testing.
type fakePublisher struct {
	mu    sync.Mutex
	count int
}

func (f *fakePublisher) Publish(_, _, _ string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.count++
}

func (f *fakePublisher) Count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.count
}

func TestMapEventToMessage_Replay(t *testing.T) {
	ev := runner.Event{Kind: runner.KindReplay, Text: "look at #42"}
	role, content, meta := MapEventToMessage("main", ev)

	assert.Equal(t, ctxbuild.RoleUser, role)
	assert.Equal(t, "look at #42", content)
	assert.Equal(t, "replay", meta["event"])
}

func TestStoreEventSink_Broadcast_ReplayUsesGuidanceReceivedType(t *testing.T) {
	broker := &capturingPublisher{}
	sink := NewStoreEventSink(nil, broker)

	sink.Broadcast(uuid.New(), runner.Event{
		Kind: runner.KindReplay,
		Text: "look at #42",
	})

	assert.Len(t, broker.events, 1)
	assert.Equal(t, "guidance_received", broker.events[0].typ)
}

// capturingPublisher captures published events for testing.
type capturingPublisher struct {
	events []struct {
		sess string
		typ  string
		data string
	}
}

func (c *capturingPublisher) Publish(sess, typ, data string) {
	c.events = append(c.events, struct {
		sess string
		typ  string
		data string
	}{sess, typ, data})
}
