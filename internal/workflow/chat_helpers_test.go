package workflow

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tphakala/alfred/internal/ctxbuild"
)

const testRecallTool = "recall_memories"

func TestSafeString(t *testing.T) {
	m := map[string]any{"action": "deploy", "count": 42}

	s, ok := safeString(m, "action")
	assert.True(t, ok)
	assert.Equal(t, "deploy", s)

	s, ok = safeString(m, "missing")
	assert.False(t, ok)
	assert.Empty(t, s)

	s, ok = safeString(m, "count")
	assert.False(t, ok)
	assert.Empty(t, s)
}

func TestToolMaxAttempts(t *testing.T) {
	idempotent := map[string]bool{testRecallTool: true, "mutating_tool": false}

	assert.Equal(t, int32(defaultRetryMaxAttempts), toolMaxAttempts(idempotent, testRecallTool),
		"an idempotent tool keeps the default retry budget")
	assert.Equal(t, int32(1), toolMaxAttempts(idempotent, "mutating_tool"),
		"a tool explicitly reported non-idempotent gets a single attempt")
	assert.Equal(t, int32(1), toolMaxAttempts(idempotent, "unknown_tool"),
		"an unknown tool name defaults to non-idempotent")
	assert.Equal(t, int32(1), toolMaxAttempts(nil, testRecallTool),
		"a nil idempotency map (e.g. registry unavailable) defaults to non-idempotent")
}

func TestEstimateTokens(t *testing.T) {
	tokens := estimateTokens("hello world")
	assert.Positive(t, tokens)
	assert.Equal(t, ctxbuild.EstimateTokens("hello world"), tokens)
}

func TestStreamTokenCount(t *testing.T) {
	t.Run("with usage", func(t *testing.T) {
		result := LLMStreamResult{
			Text:  "hello",
			Usage: &StreamTokenUsage{TotalTokens: 42},
		}
		assert.Equal(t, 42, streamTokenCount(result))
	})

	t.Run("without usage", func(t *testing.T) {
		result := LLMStreamResult{Text: "hello"}
		assert.Equal(t, estimateTokens("hello"), streamTokenCount(result))
	})
}

func TestToolTokenCount(t *testing.T) {
	msgs := []ActivityMessage{
		{TokenEstimate: 10},
		{TokenEstimate: 20},
		{TokenEstimate: 5},
	}
	assert.Equal(t, 35, toolTokenCount(msgs))
	assert.Zero(t, toolTokenCount(nil))
}

func TestBuildRoundMessages(t *testing.T) {
	t.Run("text and tools", func(t *testing.T) {
		toolMsgs := []ActivityMessage{
			{Role: ctxbuild.RoleToolCall, Content: testRecallTool},
			{Role: ctxbuild.RoleToolResult, Content: "memories"},
		}
		msgs := buildRoundMessages("hello", toolMsgs)
		require.Len(t, msgs, 3)
		assert.Equal(t, ctxbuild.RoleModel, msgs[0].Role)
		assert.Equal(t, "hello", msgs[0].Content)
		assert.Equal(t, ctxbuild.RoleToolCall, msgs[1].Role)
	})

	t.Run("tools only", func(t *testing.T) {
		toolMsgs := []ActivityMessage{
			{Role: ctxbuild.RoleToolCall, Content: testRecallTool},
		}
		msgs := buildRoundMessages("", toolMsgs)
		require.Len(t, msgs, 1)
		assert.Equal(t, ctxbuild.RoleToolCall, msgs[0].Role)
	})
}

func TestBuildToolMessages(t *testing.T) {
	call := LLMToolCall{CallID: "t1-c0", Name: testRecallTool, Args: map[string]any{"query": "test"}}
	result := &ExecToolResult{CallID: "t1-c0", Name: testRecallTool, Result: `{"memories":[]}`, Error: ""}

	msgs := buildToolMessages(call, result)
	require.Len(t, msgs, 2)

	assert.Equal(t, ctxbuild.RoleToolCall, msgs[0].Role)
	assert.Equal(t, testRecallTool, msgs[0].Content)
	assert.Equal(t, "t1-c0", msgs[0].Metadata[metaKeyCallID])

	assert.Equal(t, ctxbuild.RoleToolResult, msgs[1].Role)
	assert.JSONEq(t, `{"memories":[]}`, msgs[1].Content)
	assert.Positive(t, msgs[1].TokenEstimate)
}

func TestMarshalApprovalPayload(t *testing.T) {
	t.Run("approved", func(t *testing.T) {
		approval := &ApprovalState{Status: ApprovalStatusApproved}
		s := marshalApprovalPayload(approval)
		assert.Contains(t, s, `"approved":true`)
	})

	t.Run("rejected with reason", func(t *testing.T) {
		approval := &ApprovalState{Status: ApprovalStatusRejected, Reason: "too risky"}
		s := marshalApprovalPayload(approval)
		assert.Contains(t, s, `"approved":false`)
		assert.Contains(t, s, `"reason":"too risky"`)
	})
}

func TestCountPending(t *testing.T) {
	approvals := map[string]*ApprovalState{
		"a": {Status: ApprovalStatusPending},
		"b": {Status: ApprovalStatusApproved},
		"c": {Status: ApprovalStatusPending},
	}
	assert.Equal(t, 2, countPending(approvals))
	assert.Zero(t, countPending(nil))
}
