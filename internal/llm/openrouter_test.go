package llm

import (
	"encoding/json"
	"errors"
	"io"
	"testing"

	openrouter "github.com/revrost/go-openrouter"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMapRoleToOpenRouter verifies all role mappings to OpenRouter roles.
func TestMapRoleToOpenRouter(t *testing.T) {
	tests := []struct {
		role Role
		want string
	}{
		{RoleUser, openrouter.ChatMessageRoleUser},
		{RoleModel, openrouter.ChatMessageRoleAssistant},
		{"tool_call", openrouter.ChatMessageRoleAssistant},
		{"approval_request", openrouter.ChatMessageRoleAssistant},
		{"context", openrouter.ChatMessageRoleSystem},
		{"unknown", openrouter.ChatMessageRoleUser},
		{"other", openrouter.ChatMessageRoleUser},
		{"", openrouter.ChatMessageRoleUser},
	}
	for _, tt := range tests {
		got := mapRoleToOpenRouter(tt.role)
		assert.Equal(t, tt.want, got, "role=%q", tt.role)
	}
}

// TestToOpenRouterTools verifies a single tool declaration is converted correctly
// and that the Parameters field serializes as expected JSON.
func TestToOpenRouterTools(t *testing.T) {
	decls := []*FunctionDeclaration{
		{
			Name:        "search",
			Description: "search the web",
			Parameters: &Schema{
				Type: TypeObject,
				Properties: map[string]*Schema{
					"query": {Type: TypeString, Description: "search query"},
				},
				Required: []string{"query"},
			},
		},
	}

	tools := toOpenRouterTools(decls)
	require.Len(t, tools, 1)

	tool := tools[0]
	assert.Equal(t, openrouter.ToolTypeFunction, tool.Type)
	require.NotNil(t, tool.Function)
	assert.Equal(t, "search", tool.Function.Name)
	assert.Equal(t, "search the web", tool.Function.Description)
	require.NotNil(t, tool.Function.Parameters)

	// Verify JSON serialization round-trips correctly.
	b, err := json.Marshal(tool.Function.Parameters)
	require.NoError(t, err)

	var got map[string]any
	require.NoError(t, json.Unmarshal(b, &got))

	assert.Equal(t, "object", got["type"])
	props, ok := got["properties"].(map[string]any)
	require.True(t, ok, "properties should be an object")
	_, hasQuery := props["query"]
	assert.True(t, hasQuery, "should have query property")
}

// TestToOpenRouterTools_Nil verifies nil input returns nil.
func TestToOpenRouterTools_Nil(t *testing.T) {
	assert.Nil(t, toOpenRouterTools(nil))
}

// TestToOpenRouterToolChoice verifies all tool choice mappings.
func TestToOpenRouterToolChoice(t *testing.T) {
	assert.Nil(t, toOpenRouterToolChoice(nil))
	assert.Equal(t, "auto", toOpenRouterToolChoice(&ToolConfig{Mode: ToolModeAuto}))
	assert.Equal(t, "required", toOpenRouterToolChoice(&ToolConfig{Mode: ToolModeAny}))
	assert.Equal(t, "none", toOpenRouterToolChoice(&ToolConfig{Mode: ToolModeNone}))
}

// TestMessagesToOpenRouterMessages_BasicRoles verifies user and model messages
// are mapped to the correct OpenRouter roles.
func TestMessagesToOpenRouterMessages_BasicRoles(t *testing.T) {
	messages := []Message{
		{Role: RoleUser, Text: "hello"},
		{Role: RoleModel, Text: "hi there"},
	}

	out := messagesToOpenRouterMessages(messages, "")
	require.Len(t, out, 2)

	assert.Equal(t, openrouter.ChatMessageRoleUser, out[0].Role)
	assert.Equal(t, "hello", out[0].Content.Text)

	assert.Equal(t, openrouter.ChatMessageRoleAssistant, out[1].Role)
	assert.Equal(t, "hi there", out[1].Content.Text)
}

// TestMessagesToOpenRouterMessages_SystemInstruction verifies the system
// instruction is prepended as the first message.
func TestMessagesToOpenRouterMessages_SystemInstruction(t *testing.T) {
	messages := []Message{
		{Role: RoleUser, Text: "hello"},
	}

	out := messagesToOpenRouterMessages(messages, "You are Alfred.")
	require.Len(t, out, 2)

	assert.Equal(t, openrouter.ChatMessageRoleSystem, out[0].Role)
	assert.Equal(t, "You are Alfred.", out[0].Content.Text)

	assert.Equal(t, openrouter.ChatMessageRoleUser, out[1].Role)
}

// TestMessagesToOpenRouterMessages_WithToolCalls verifies a model message with
// ToolCalls is converted to an assistant message with tool_calls set.
func TestMessagesToOpenRouterMessages_WithToolCalls(t *testing.T) {
	messages := []Message{
		{
			Role: RoleModel,
			Text: "calling tool",
			ToolCalls: []ToolCallPart{
				{
					CallID: "call-1",
					Name:   "do_thing",
					Args:   map[string]any{"key": "value"},
				},
			},
		},
	}

	out := messagesToOpenRouterMessages(messages, "")
	require.Len(t, out, 1)

	msg := out[0]
	assert.Equal(t, openrouter.ChatMessageRoleAssistant, msg.Role)
	assert.Equal(t, "calling tool", msg.Content.Text)
	require.Len(t, msg.ToolCalls, 1)

	tc := msg.ToolCalls[0]
	assert.Equal(t, "call-1", tc.ID)
	assert.Equal(t, openrouter.ToolTypeFunction, tc.Type)
	assert.Equal(t, "do_thing", tc.Function.Name)

	var args map[string]any
	require.NoError(t, json.Unmarshal([]byte(tc.Function.Arguments), &args))
	assert.Equal(t, "value", args["key"])
}

// TestMessagesToOpenRouterMessages_WithToolResults verifies a message with
// ToolResults is expanded into individual tool messages.
func TestMessagesToOpenRouterMessages_WithToolResults(t *testing.T) {
	messages := []Message{
		{
			Role: RoleUser,
			ToolResults: []ToolResultPart{
				{CallID: "call-1", Name: "do_thing", Result: "ok"},
				{CallID: "call-2", Name: "other", Error: "timeout"},
			},
		},
	}

	out := messagesToOpenRouterMessages(messages, "")
	require.Len(t, out, 2)

	assert.Equal(t, openrouter.ChatMessageRoleTool, out[0].Role)
	assert.Equal(t, "call-1", out[0].ToolCallID)
	assert.Equal(t, "ok", out[0].Content.Text)

	assert.Equal(t, openrouter.ChatMessageRoleTool, out[1].Role)
	assert.Equal(t, "call-2", out[1].ToolCallID)
	assert.Equal(t, "timeout", out[1].Content.Text)
}

// TestOpenRouterToolResultString verifies formatting for all result types.
func TestOpenRouterToolResultString(t *testing.T) {
	t.Run("string result", func(t *testing.T) {
		tr := ToolResultPart{Result: "hello"}
		assert.Equal(t, "hello", openRouterToolResultString(tr))
	})

	t.Run("error", func(t *testing.T) {
		tr := ToolResultPart{Error: "something failed"}
		assert.Equal(t, "something failed", openRouterToolResultString(tr))
	})

	t.Run("map result", func(t *testing.T) {
		tr := ToolResultPart{Result: map[string]any{"key": "val"}}
		got := openRouterToolResultString(tr)
		var m map[string]any
		require.NoError(t, json.Unmarshal([]byte(got), &m))
		assert.Equal(t, "val", m["key"])
	})

	t.Run("nil result", func(t *testing.T) {
		tr := ToolResultPart{Result: nil}
		assert.Empty(t, openRouterToolResultString(tr))
	})
}

// ---------------------------------------------------------------------------
// Streaming test helpers
// ---------------------------------------------------------------------------

// mockOpenRouterStream implements chatCompletionStream for testing. It returns
// pre-configured responses in order, then io.EOF.
type mockOpenRouterStream struct {
	responses []openrouter.ChatCompletionStreamResponse
	pos       int
	closed    bool
}

func (m *mockOpenRouterStream) Recv() (openrouter.ChatCompletionStreamResponse, error) {
	if m.pos >= len(m.responses) {
		return openrouter.ChatCompletionStreamResponse{}, io.EOF
	}
	resp := m.responses[m.pos]
	m.pos++
	return resp, nil
}

func (m *mockOpenRouterStream) Close() {
	m.closed = true
}

// intPtr returns a pointer to the given int, useful for ToolCall.Index.
func intPtr(i int) *int {
	return &i
}

// errorAfterNStream wraps a chatCompletionStream and returns the given error
// after N successful Recv calls.
type errorAfterNStream struct {
	inner chatCompletionStream
	n     int
	count int
	err   error
}

func (e *errorAfterNStream) Recv() (openrouter.ChatCompletionStreamResponse, error) {
	if e.count >= e.n {
		return openrouter.ChatCompletionStreamResponse{}, e.err
	}
	e.count++
	return e.inner.Recv()
}

func (e *errorAfterNStream) Close() {
	e.inner.Close()
}

// ---------------------------------------------------------------------------
// Streaming tests
// ---------------------------------------------------------------------------

// TestOpenRouterStreamIterator_TextOnly verifies that two text-only chunks
// stream through immediately, followed by io.EOF.
func TestOpenRouterStreamIterator_TextOnly(t *testing.T) {
	stream := &mockOpenRouterStream{
		responses: []openrouter.ChatCompletionStreamResponse{
			{
				Choices: []openrouter.ChatCompletionStreamChoice{
					{Delta: openrouter.ChatCompletionStreamChoiceDelta{Content: "Hello"}},
				},
			},
			{
				Choices: []openrouter.ChatCompletionStreamChoice{
					{Delta: openrouter.ChatCompletionStreamChoiceDelta{Content: " world"}},
				},
			},
		},
	}

	it := newOpenRouterStreamIterator(stream)
	defer func() { _ = it.Close() }()

	chunk, err := it.Next()
	require.NoError(t, err)
	assert.Equal(t, "Hello", chunk.Text)

	chunk, err = it.Next()
	require.NoError(t, err)
	assert.Equal(t, " world", chunk.Text)

	_, err = it.Next()
	assert.ErrorIs(t, err, io.EOF)
}

// TestOpenRouterStreamIterator_ToolCallAccumulation verifies that fragmented
// tool calls spread across multiple SSE chunks are accumulated and emitted
// as a single complete ToolCallPart after the stream ends.
func TestOpenRouterStreamIterator_ToolCallAccumulation(t *testing.T) {
	stream := &mockOpenRouterStream{
		responses: []openrouter.ChatCompletionStreamResponse{
			// First chunk: ID, name, and partial arguments.
			{
				Choices: []openrouter.ChatCompletionStreamChoice{
					{
						Delta: openrouter.ChatCompletionStreamChoiceDelta{
							ToolCalls: []openrouter.ToolCall{
								{
									Index: intPtr(0),
									ID:    "call-abc",
									Type:  openrouter.ToolTypeFunction,
									Function: openrouter.FunctionCall{
										Name:      "search",
										Arguments: `{"que`,
									},
								},
							},
						},
					},
				},
			},
			// Second chunk: more argument fragments.
			{
				Choices: []openrouter.ChatCompletionStreamChoice{
					{
						Delta: openrouter.ChatCompletionStreamChoiceDelta{
							ToolCalls: []openrouter.ToolCall{
								{
									Index: intPtr(0),
									Function: openrouter.FunctionCall{
										Arguments: `ry":"hello"}`,
									},
								},
							},
						},
					},
				},
			},
			// Third chunk: finish reason only.
			{
				Choices: []openrouter.ChatCompletionStreamChoice{
					{
						FinishReason: openrouter.FinishReasonToolCalls,
					},
				},
			},
		},
	}

	it := newOpenRouterStreamIterator(stream)
	defer func() { _ = it.Close() }()

	// The first Next after stream end should emit tool calls.
	chunk, err := it.Next()
	require.NoError(t, err)
	require.Len(t, chunk.ToolCalls, 1)

	tc := chunk.ToolCalls[0]
	assert.Equal(t, "call-abc", tc.CallID)
	assert.Equal(t, "search", tc.Name)
	assert.Equal(t, "hello", tc.Args["query"])

	// Then EOF.
	_, err = it.Next()
	assert.ErrorIs(t, err, io.EOF)
}

// TestOpenRouterStreamIterator_MultipleToolCalls verifies that a single chunk
// containing two tool calls at different indices produces two ToolCallParts.
func TestOpenRouterStreamIterator_MultipleToolCalls(t *testing.T) {
	stream := &mockOpenRouterStream{
		responses: []openrouter.ChatCompletionStreamResponse{
			{
				Choices: []openrouter.ChatCompletionStreamChoice{
					{
						Delta: openrouter.ChatCompletionStreamChoiceDelta{
							ToolCalls: []openrouter.ToolCall{
								{
									Index: intPtr(0),
									ID:    "call-1",
									Type:  openrouter.ToolTypeFunction,
									Function: openrouter.FunctionCall{
										Name:      "search",
										Arguments: `{"q":"a"}`,
									},
								},
								{
									Index: intPtr(1),
									ID:    "call-2",
									Type:  openrouter.ToolTypeFunction,
									Function: openrouter.FunctionCall{
										Name:      "lookup",
										Arguments: `{"id":42}`,
									},
								},
							},
						},
					},
				},
			},
		},
	}

	it := newOpenRouterStreamIterator(stream)
	defer func() { _ = it.Close() }()

	chunk, err := it.Next()
	require.NoError(t, err)
	require.Len(t, chunk.ToolCalls, 2)

	assert.Equal(t, "call-1", chunk.ToolCalls[0].CallID)
	assert.Equal(t, "search", chunk.ToolCalls[0].Name)
	assert.Equal(t, "a", chunk.ToolCalls[0].Args["q"])

	assert.Equal(t, "call-2", chunk.ToolCalls[1].CallID)
	assert.Equal(t, "lookup", chunk.ToolCalls[1].Name)
	assert.EqualValues(t, 42, chunk.ToolCalls[1].Args["id"])

	_, err = it.Next()
	assert.ErrorIs(t, err, io.EOF)
}

// TestOpenRouterStreamIterator_TextThenToolCalls verifies that text is emitted
// first during Phase 1, and tool calls are emitted in Phase 2 after the stream
// ends.
func TestOpenRouterStreamIterator_TextThenToolCalls(t *testing.T) {
	stream := &mockOpenRouterStream{
		responses: []openrouter.ChatCompletionStreamResponse{
			// Text chunk.
			{
				Choices: []openrouter.ChatCompletionStreamChoice{
					{Delta: openrouter.ChatCompletionStreamChoiceDelta{Content: "Let me search"}},
				},
			},
			// Tool call chunk.
			{
				Choices: []openrouter.ChatCompletionStreamChoice{
					{
						Delta: openrouter.ChatCompletionStreamChoiceDelta{
							ToolCalls: []openrouter.ToolCall{
								{
									Index: intPtr(0),
									ID:    "call-x",
									Type:  openrouter.ToolTypeFunction,
									Function: openrouter.FunctionCall{
										Name:      "web_search",
										Arguments: `{"query":"test"}`,
									},
								},
							},
						},
					},
				},
			},
		},
	}

	it := newOpenRouterStreamIterator(stream)
	defer func() { _ = it.Close() }()

	// First: text.
	chunk, err := it.Next()
	require.NoError(t, err)
	assert.Equal(t, "Let me search", chunk.Text)
	assert.Nil(t, chunk.ToolCalls)

	// Second: tool calls.
	chunk, err = it.Next()
	require.NoError(t, err)
	require.Len(t, chunk.ToolCalls, 1)
	assert.Equal(t, "call-x", chunk.ToolCalls[0].CallID)
	assert.Equal(t, "web_search", chunk.ToolCalls[0].Name)
	assert.Equal(t, "test", chunk.ToolCalls[0].Args["query"])

	_, err = it.Next()
	assert.ErrorIs(t, err, io.EOF)
}

// TestOpenRouterStreamIterator_Usage verifies that a text chunk followed by a
// usage-only chunk emits text first, then usage tokens.
func TestOpenRouterStreamIterator_Usage(t *testing.T) {
	stream := &mockOpenRouterStream{
		responses: []openrouter.ChatCompletionStreamResponse{
			{
				Choices: []openrouter.ChatCompletionStreamChoice{
					{Delta: openrouter.ChatCompletionStreamChoiceDelta{Content: "Hi"}},
				},
			},
			// Usage-only chunk (no choices content, just usage metadata).
			{
				Usage: &openrouter.Usage{
					PromptTokens:     10,
					CompletionTokens: 5,
					TotalTokens:      15,
				},
			},
		},
	}

	it := newOpenRouterStreamIterator(stream)
	defer func() { _ = it.Close() }()

	// First: text.
	chunk, err := it.Next()
	require.NoError(t, err)
	assert.Equal(t, "Hi", chunk.Text)

	// Second: usage.
	chunk, err = it.Next()
	require.NoError(t, err)
	require.NotNil(t, chunk.Tokens)
	assert.Equal(t, int32(10), chunk.Tokens.PromptTokens)
	assert.Equal(t, int32(5), chunk.Tokens.ResponseTokens)
	assert.Equal(t, int32(15), chunk.Tokens.TotalTokens)

	_, err = it.Next()
	assert.ErrorIs(t, err, io.EOF)
}

// TestOpenRouterStreamIterator_EmptyStream verifies that an empty stream
// (immediate EOF) returns io.EOF on the first Next call.
func TestOpenRouterStreamIterator_EmptyStream(t *testing.T) {
	stream := &mockOpenRouterStream{
		responses: nil,
	}

	it := newOpenRouterStreamIterator(stream)
	defer func() { _ = it.Close() }()

	_, err := it.Next()
	assert.ErrorIs(t, err, io.EOF)
}

// TestOpenRouterStreamIterator_IndexOutOfRange verifies that a tool call with
// an index exceeding maxToolCallIndex returns an error.
func TestOpenRouterStreamIterator_IndexOutOfRange(t *testing.T) {
	stream := &mockOpenRouterStream{
		responses: []openrouter.ChatCompletionStreamResponse{
			{
				Choices: []openrouter.ChatCompletionStreamChoice{
					{
						Delta: openrouter.ChatCompletionStreamChoiceDelta{
							ToolCalls: []openrouter.ToolCall{
								{
									Index: intPtr(999999),
									ID:    "call-bad",
									Function: openrouter.FunctionCall{
										Name:      "exploit",
										Arguments: `{}`,
									},
								},
							},
						},
					},
				},
			},
		},
	}

	it := newOpenRouterStreamIterator(stream)
	defer func() { _ = it.Close() }()

	_, err := it.Next()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "out of range")
}

// TestOpenRouterStreamIterator_CloseIdempotent verifies that Close can be
// called multiple times without error.
func TestOpenRouterStreamIterator_CloseIdempotent(t *testing.T) {
	stream := &mockOpenRouterStream{
		responses: nil,
	}

	it := newOpenRouterStreamIterator(stream)

	err := it.Close()
	require.NoError(t, err)
	assert.True(t, stream.closed)

	// Second close should also succeed.
	err = it.Close()
	require.NoError(t, err)
}

// TestOpenRouterStreamIterator_ErrorPropagation verifies that a stream error
// after the first chunk is propagated, and subsequent Next calls return io.EOF.
func TestOpenRouterStreamIterator_ErrorPropagation(t *testing.T) {
	inner := &mockOpenRouterStream{
		responses: []openrouter.ChatCompletionStreamResponse{
			{
				Choices: []openrouter.ChatCompletionStreamChoice{
					{Delta: openrouter.ChatCompletionStreamChoiceDelta{Content: "partial"}},
				},
			},
			// Second response exists but will never be reached.
			{
				Choices: []openrouter.ChatCompletionStreamChoice{
					{Delta: openrouter.ChatCompletionStreamChoiceDelta{Content: "never"}},
				},
			},
		},
	}

	streamErr := errors.New("connection reset")
	errStream := &errorAfterNStream{
		inner: inner,
		n:     1,
		err:   streamErr,
	}

	it := newOpenRouterStreamIterator(errStream)
	defer func() { _ = it.Close() }()

	// First chunk succeeds.
	chunk, err := it.Next()
	require.NoError(t, err)
	assert.Equal(t, "partial", chunk.Text)

	// Second call gets the error.
	_, err = it.Next()
	require.ErrorIs(t, err, streamErr)

	// Subsequent calls return io.EOF (iterator is closed).
	_, err = it.Next()
	require.ErrorIs(t, err, io.EOF)
}
