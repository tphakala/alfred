package workflow_test

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/testsuite"

	"github.com/tphakala/alfred/internal/agent"
	"github.com/tphakala/alfred/internal/llm"
	"github.com/tphakala/alfred/internal/memory"
	"github.com/tphakala/alfred/internal/store"
	wf "github.com/tphakala/alfred/internal/workflow"
)

// Test string constants to avoid goconst lint warnings for repeated literals.
const (
	testRoleUser           = "user"
	testRoleModel          = "model"
	testCallIDVal          = "callId"
	testHello              = "hello"
	testActionName         = "deploy"
	testDeployMsg          = "deploy it"
	testDeployProdMsg      = "deploy to prod"
	testToolRecall         = "recall_memories"
	testIdempotentToolName = "idempotent_tool"
)

// mockMemoryClient implements memory.MemoryClient for tests.
type mockMemoryClient struct {
	recallResp *memory.RecallResponse
	recallErr  error
	retainErr  error
	retainReqs []memory.RetainRequest
}

func (m *mockMemoryClient) Recall(_ context.Context, _ string) (*memory.RecallResponse, error) {
	return m.recallResp, m.recallErr
}

func (m *mockMemoryClient) Retain(_ context.Context, req memory.RetainRequest) error { //nolint:gocritic // interface compliance
	m.retainReqs = append(m.retainReqs, req)
	return m.retainErr
}

func TestExtractEpochSummary(t *testing.T) {
	suite := &testsuite.WorkflowTestSuite{}
	env := suite.NewTestActivityEnvironment()

	extractJSON := `{"resolved":["fixed bug #42"],"pending":["deploy pending"],"decisions":["use postgres"],"context":["migration in progress"]}`

	mockLLM := &llm.MockClient{
		Response: extractJSON,
	}

	activities := &wf.ChatActivities{
		LLM:       mockLLM,
		ChatModel: "test-model",
	}
	env.RegisterActivity(activities)

	req := wf.ExtractRequest{
		SessionID: uuid.New(),
		Messages: []wf.ContextMessage{
			{Role: "user", Content: "fix bug 42"},
			{Role: "model", Content: "I fixed bug 42"},
		},
	}

	val, err := env.ExecuteActivity(activities.ExtractEpochSummary, req)
	require.NoError(t, err)

	var result wf.ExtractResult
	require.NoError(t, val.Get(&result))

	require.NotEmpty(t, result.Summary)
	require.JSONEq(t, extractJSON, result.ExtractedFacts)
}

func TestRetainToHindsight_Success(t *testing.T) {
	suite := &testsuite.WorkflowTestSuite{}
	env := suite.NewTestActivityEnvironment()

	mockMem := &mockMemoryClient{}
	activities := &wf.ChatActivities{
		Memory: mockMem,
		Logger: slog.Default(),
	}
	env.RegisterActivity(activities)

	req := wf.RetainHindsightRequest{
		SessionID:      uuid.New(),
		ExtractedFacts: `{"resolved":["task done"]}`,
		Epoch:          1,
	}

	_, err := env.ExecuteActivity(activities.RetainToHindsight, req)
	require.NoError(t, err)
	require.Len(t, mockMem.retainReqs, 1)
	require.Contains(t, mockMem.retainReqs[0].Content, "task done")
}

func TestRetainToHindsight_ErrorSwallowed(t *testing.T) {
	suite := &testsuite.WorkflowTestSuite{}
	env := suite.NewTestActivityEnvironment()

	mockMem := &mockMemoryClient{
		retainErr: fmt.Errorf("hindsight down"),
	}
	activities := &wf.ChatActivities{
		Memory: mockMem,
		Logger: slog.Default(),
	}
	env.RegisterActivity(activities)

	req := wf.RetainHindsightRequest{
		SessionID:      uuid.New(),
		ExtractedFacts: "some facts",
		Epoch:          1,
	}

	_, err := env.ExecuteActivity(activities.RetainToHindsight, req)
	require.NoError(t, err, "Hindsight errors should be swallowed, not propagated")
}

func TestPersistEpochTransition_NilStore(t *testing.T) {
	suite := &testsuite.WorkflowTestSuite{}
	env := suite.NewTestActivityEnvironment()

	activities := &wf.ChatActivities{}
	env.RegisterActivity(activities)

	req := wf.PersistEpochTransitionRequest{
		FromSessionID:  uuid.New(),
		ToSessionID:    uuid.New(),
		Trigger:        "token_threshold",
		ExtractedFacts: "facts",
		Summary:        "summary",
	}

	_, err := env.ExecuteActivity(activities.PersistEpochTransition, req)
	require.Error(t, err, "should fail with nil store")
}

func TestUpdateSessionEpochActivity_NilStore(t *testing.T) {
	suite := &testsuite.WorkflowTestSuite{}
	env := suite.NewTestActivityEnvironment()

	activities := &wf.ChatActivities{}
	env.RegisterActivity(activities)

	_, err := env.ExecuteActivity(activities.UpdateSessionEpochActivity, uuid.New(), 2, "summary")
	require.Error(t, err, "should fail with nil store")
}

func TestStoredToHistory_TextOnly(t *testing.T) {
	msgs := []store.Message{
		{Role: testRoleUser, Content: testHello},
		{Role: testRoleModel, Content: "hi there"},
	}
	history, err := wf.StoredToHistory(msgs)
	require.NoError(t, err)
	require.Len(t, history, 2)
	assert.Equal(t, agent.EntryText, history[0].Kind)
	assert.Equal(t, agent.RoleUser, history[0].Role)
	assert.Equal(t, testHello, history[0].Text)
	assert.Equal(t, agent.EntryText, history[1].Kind)
	assert.Equal(t, agent.RoleModel, history[1].Role)
}

func TestStoredToHistory_ToolExchange(t *testing.T) {
	callMeta, err := json.Marshal(map[string]any{
		testCallIDVal: "t1-c0",
		"args":        map[string]any{"query": "test"},
	})
	require.NoError(t, err)
	resultMeta, err := json.Marshal(map[string]any{
		testCallIDVal: "t1-c0",
		"name":        testToolRecall,
	})
	require.NoError(t, err)
	msgs := []store.Message{
		{Role: testRoleUser, Content: "recall something"},
		{Role: testRoleModel, Content: "Let me search"},
		{Role: "tool_call", Content: testToolRecall, Metadata: callMeta},
		{Role: "tool_result", Content: `{"memories":["fact1"]}`, Metadata: resultMeta},
		{Role: testRoleModel, Content: "I found it"},
	}
	history, err := wf.StoredToHistory(msgs)
	require.NoError(t, err)
	require.Len(t, history, 4) // user, model, tool_exchange, model
	assert.Equal(t, agent.EntryText, history[0].Kind)
	assert.Equal(t, agent.EntryText, history[1].Kind)
	assert.Equal(t, agent.EntryToolExchange, history[2].Kind)
	assert.Equal(t, testToolRecall, history[2].ToolCall.Name)
	assert.Equal(t, "t1-c0", history[2].ToolCall.CallID)
	assert.Equal(t, agent.EntryText, history[3].Kind)
}

func TestStoredToHistory_SkipsContextRole(t *testing.T) {
	msgs := []store.Message{
		{Role: "context", Content: "epoch summary"},
		{Role: testRoleUser, Content: testHello},
	}
	history, err := wf.StoredToHistory(msgs)
	require.NoError(t, err)
	require.Len(t, history, 1)
	assert.Equal(t, testHello, history[0].Text)
}

func TestStoredToHistory_ApprovalExchange(t *testing.T) {
	reqMeta, err := json.Marshal(map[string]any{
		testCallIDVal: "sess:abc123",
		"action":      testActionName,
	})
	require.NoError(t, err)
	resMeta, err := json.Marshal(map[string]any{
		testCallIDVal: "sess:abc123",
	})
	require.NoError(t, err)
	msgs := []store.Message{
		{Role: testRoleUser, Content: testDeployMsg},
		{Role: testRoleModel, Content: "I will deploy"},
		{Role: "approval_request", Content: testDeployProdMsg, Metadata: reqMeta},
		{Role: "approval_result", Content: `{"approved":true}`, Metadata: resMeta},
	}
	history, err := wf.StoredToHistory(msgs)
	require.NoError(t, err)
	require.Len(t, history, 3) // user, model, tool_exchange
	assert.Equal(t, agent.EntryToolExchange, history[2].Kind)
	assert.Equal(t, "request_approval", history[2].ToolCall.Name)
}

func TestStoredToHistory_DanglingApprovalRequest(t *testing.T) {
	reqMeta, err := json.Marshal(map[string]any{
		testCallIDVal: "sess:pending",
		"action":      testActionName,
	})
	require.NoError(t, err)
	msgs := []store.Message{
		{Role: testRoleUser, Content: testDeployMsg},
		{Role: testRoleModel, Content: "I will deploy"},
		{Role: "approval_request", Content: testDeployProdMsg, Metadata: reqMeta},
	}
	history, err := wf.StoredToHistory(msgs)
	require.NoError(t, err)
	require.Len(t, history, 2) // user, model — dangling approval_request is dropped
	assert.Equal(t, agent.EntryText, history[0].Kind)
	assert.Equal(t, agent.EntryText, history[1].Kind)
}

func TestStoredToHistory_OrphanedToolResult(t *testing.T) {
	resultMeta, err := json.Marshal(map[string]any{
		testCallIDVal: "orphan-1",
		"name":        testToolRecall,
	})
	require.NoError(t, err)
	msgs := []store.Message{
		{Role: testRoleUser, Content: testHello},
		{Role: "tool_result", Content: `{"memories":["fact1"]}`, Metadata: resultMeta},
		{Role: testRoleModel, Content: "here is the result"},
	}
	history, err := wf.StoredToHistory(msgs)
	require.NoError(t, err)
	require.Len(t, history, 2) // user, model — orphaned tool_result skipped
	assert.Equal(t, agent.EntryText, history[0].Kind)
	assert.Equal(t, agent.EntryText, history[1].Kind)
}

func TestStoredToHistory_OrphanedApprovalResult(t *testing.T) {
	resMeta, err := json.Marshal(map[string]any{
		testCallIDVal: "orphan-2",
	})
	require.NoError(t, err)
	msgs := []store.Message{
		{Role: testRoleUser, Content: testDeployMsg},
		{Role: "approval_result", Content: `{"approved":true}`, Metadata: resMeta},
		{Role: testRoleModel, Content: "done"},
	}
	history, err := wf.StoredToHistory(msgs)
	require.NoError(t, err)
	require.Len(t, history, 2) // user, model — orphaned approval_result skipped
	assert.Equal(t, agent.EntryText, history[0].Kind)
	assert.Equal(t, agent.EntryText, history[1].Kind)
}

func TestStoredToHistory_EmptyHistory(t *testing.T) {
	history, err := wf.StoredToHistory(nil)
	require.NoError(t, err)
	assert.Empty(t, history)
}

// fakeBroker implements wf.SessionEventPublisher, recording every published
// event for assertions.
type fakeBroker struct {
	events []publishedEvent
}

type publishedEvent struct {
	sessionID string
	eventType string
	data      string
}

func (f *fakeBroker) Publish(sessionID, eventType, data string) {
	f.events = append(f.events, publishedEvent{sessionID: sessionID, eventType: eventType, data: data})
}

func TestChatActivities_LLMStream_NilLLMClient(t *testing.T) {
	suite := &testsuite.WorkflowTestSuite{}
	env := suite.NewTestActivityEnvironment()

	activities := &wf.ChatActivities{}
	env.RegisterActivity(activities)

	_, err := env.ExecuteActivity(activities.LLMStream, wf.LLMStreamRequest{SessionID: uuid.New()})
	require.Error(t, err)
}

func TestChatActivities_LLMStream_AccumulatesTextAndToolCalls(t *testing.T) {
	suite := &testsuite.WorkflowTestSuite{}
	env := suite.NewTestActivityEnvironment()

	mockLLM := &llm.MockClient{
		MockStream: []llm.StreamChunk{
			{Text: "Hello "},
			{
				Text: "world",
				ToolCalls: []llm.ToolCallPart{
					{Name: testToolRecall, Args: map[string]any{"query": "test"}},
				},
			},
			{Tokens: &llm.TokenUsage{PromptTokens: 5, ResponseTokens: 7, TotalTokens: 12}},
		},
	}

	activities := &wf.ChatActivities{
		LLM:       mockLLM,
		ChatModel: "test-model",
	}
	env.RegisterActivity(activities)

	req := wf.LLMStreamRequest{
		SessionID: uuid.New(),
		TurnID:    1,
		Round:     3,
		Messages:  []wf.ContextMessage{{Role: testRoleUser, Content: testHello}},
	}

	val, err := env.ExecuteActivity(activities.LLMStream, req)
	require.NoError(t, err)

	var result wf.LLMStreamResult
	require.NoError(t, val.Get(&result))

	assert.Equal(t, "Hello world", result.Text)
	require.Len(t, result.ToolCalls, 1)
	assert.Equal(t, "t3-c0", result.ToolCalls[0].CallID, "CallID must follow the t{round}-c{index} scheme")
	assert.Equal(t, testToolRecall, result.ToolCalls[0].Name)
	assert.Equal(t, "test", result.ToolCalls[0].Args["query"])
	require.NotNil(t, result.Usage)
	assert.Equal(t, int32(12), result.Usage.TotalTokens)
}

func TestChatActivities_LLMStream_PrependsConfiguredSystemPrompt(t *testing.T) {
	suite := &testsuite.WorkflowTestSuite{}
	env := suite.NewTestActivityEnvironment()

	mockLLM := &llm.MockClient{}
	activities := &wf.ChatActivities{
		LLM:          mockLLM,
		ChatModel:    "test-model",
		SystemPrompt: "You are the chat agent.",
	}
	env.RegisterActivity(activities)

	req := wf.LLMStreamRequest{
		SessionID:         uuid.New(),
		Round:             1,
		Messages:          []wf.ContextMessage{{Role: testRoleUser, Content: testHello}},
		SystemInstruction: "Epoch summary context.",
	}

	_, err := env.ExecuteActivity(activities.LLMStream, req)
	require.NoError(t, err)

	require.NotNil(t, mockLLM.LastStreamRequest)
	assert.Equal(t, "You are the chat agent.\n\nEpoch summary context.", mockLLM.LastStreamRequest.SystemInstruction,
		"chat's baked-in SystemPrompt must be prepended to the workflow-provided instruction")
}

func TestChatActivities_LLMStream_EmitsChunkSSE(t *testing.T) {
	suite := &testsuite.WorkflowTestSuite{}
	env := suite.NewTestActivityEnvironment()

	mockLLM := &llm.MockClient{
		MockStream: []llm.StreamChunk{{Text: testHello}},
	}
	broker := &fakeBroker{}
	activities := &wf.ChatActivities{
		LLM:           mockLLM,
		ChatModel:     "test-model",
		SessionBroker: broker,
	}
	env.RegisterActivity(activities)

	sessionID := uuid.New()
	req := wf.LLMStreamRequest{
		SessionID: sessionID,
		TurnID:    2,
		Round:     1,
		Messages:  []wf.ContextMessage{{Role: testRoleUser, Content: testHello}},
	}

	_, err := env.ExecuteActivity(activities.LLMStream, req)
	require.NoError(t, err)

	require.Len(t, broker.events, 1)
	assert.Equal(t, sessionID.String(), broker.events[0].sessionID)
	assert.Equal(t, "chunk", broker.events[0].eventType)
	assert.Contains(t, broker.events[0].data, testHello)
}

// fakeIdempotencyTool implements agent.Tool with a configurable Idempotent
// result, for ToolIdempotency tests.
type fakeIdempotencyTool struct {
	name       string
	idempotent bool
}

func (f *fakeIdempotencyTool) Name() string { return f.name }
func (f *fakeIdempotencyTool) Declaration() *llm.FunctionDeclaration {
	return &llm.FunctionDeclaration{Name: f.name}
}
func (f *fakeIdempotencyTool) Execute(_ context.Context, _ map[string]any) (any, error) {
	return map[string]any{}, nil
}
func (f *fakeIdempotencyTool) Idempotent() bool { return f.idempotent }

func TestChatActivities_ToolIdempotency_NilRegistry(t *testing.T) {
	suite := &testsuite.WorkflowTestSuite{}
	env := suite.NewTestActivityEnvironment()

	activities := &wf.ChatActivities{} // nil Registry
	env.RegisterActivity(activities)

	val, err := env.ExecuteActivity(activities.ToolIdempotency, []string{"anything"})
	require.NoError(t, err, "a nil registry must not fail the activity")

	var result map[string]bool
	require.NoError(t, val.Get(&result))
	assert.Empty(t, result)
}

func TestChatActivities_ToolIdempotency_QueriesRegistry(t *testing.T) {
	suite := &testsuite.WorkflowTestSuite{}
	env := suite.NewTestActivityEnvironment()

	registry := agent.NewRegistry(
		&fakeIdempotencyTool{name: testIdempotentToolName, idempotent: true},
		&fakeIdempotencyTool{name: "mutating_tool", idempotent: false},
	)
	activities := &wf.ChatActivities{Registry: registry}
	env.RegisterActivity(activities)

	val, err := env.ExecuteActivity(activities.ToolIdempotency,
		[]string{testIdempotentToolName, "mutating_tool", "unknown_tool"})
	require.NoError(t, err)

	var result map[string]bool
	require.NoError(t, val.Get(&result))
	assert.True(t, result[testIdempotentToolName])
	assert.False(t, result["mutating_tool"])
	assert.False(t, result["unknown_tool"], "an unregistered tool defaults to non-idempotent")
}

func TestChatActivities_ToolIdempotency_EmptyNamesQueriesAllRegisteredTools(t *testing.T) {
	suite := &testsuite.WorkflowTestSuite{}
	env := suite.NewTestActivityEnvironment()

	registry := agent.NewRegistry(&fakeIdempotencyTool{name: testIdempotentToolName, idempotent: true})
	activities := &wf.ChatActivities{Registry: registry}
	env.RegisterActivity(activities)

	val, err := env.ExecuteActivity(activities.ToolIdempotency, []string(nil))
	require.NoError(t, err)

	var result map[string]bool
	require.NoError(t, val.Get(&result))
	assert.True(t, result[testIdempotentToolName])
}
