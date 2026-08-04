package agent

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tphakala/alfred/internal/llm"
	"github.com/tphakala/alfred/internal/memory"
)

// Test constants for repeated string literals.
const (
	testToolRecall  = "recall_memories"
	testArgQuery    = "query"
	testArgMemories = "memories"
)

func TestIsNewConversation(t *testing.T) {
	tests := []struct {
		name    string
		history []HistoryEntry
		want    bool
	}{
		{"empty", nil, false},
		{"one user text", []HistoryEntry{{Kind: EntryText, Role: RoleUser, Text: "hi"}}, true},
		{"one model text", []HistoryEntry{{Kind: EntryText, Role: RoleModel, Text: "hi"}}, false},
		{
			"user then model",
			[]HistoryEntry{
				{Kind: EntryText, Role: RoleUser, Text: "a"},
				{Kind: EntryText, Role: RoleModel, Text: "b"},
			},
			false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, IsNewConversation(tt.history))
		})
	}
}

func TestFirstUserText(t *testing.T) {
	h := []HistoryEntry{
		{Kind: EntryText, Role: RoleModel, Text: "ignore"},
		{Kind: EntryText, Role: RoleUser, Text: "want this"},
	}
	assert.Equal(t, "want this", FirstUserText(h))
}

func TestFirstUserText_Empty(t *testing.T) {
	assert.Empty(t, FirstUserText(nil))
}

func TestFormatRecallAsSystemInstruction(t *testing.T) {
	m := []memory.MemoryItem{
		{Content: "fact one"},
		{Content: "fact two"},
	}
	s := FormatRecallAsSystemInstruction(m)
	assert.Contains(t, s, "Prior knowledge from memory")
	assert.Contains(t, s, "- fact one")
	assert.Contains(t, s, "- fact two")
}

func TestFormatRecallAsSystemInstruction_Empty(t *testing.T) {
	assert.Empty(t, FormatRecallAsSystemInstruction(nil))
}

func TestHistoryToMessages_PlainTextAlternation(t *testing.T) {
	h := []HistoryEntry{
		{Kind: EntryText, Role: RoleUser, Text: "q1"},
		{Kind: EntryText, Role: RoleModel, Text: "a1"},
		{Kind: EntryText, Role: RoleUser, Text: "q2"},
	}
	msgs, err := HistoryToLLMMessages(h)
	require.NoError(t, err)
	require.Len(t, msgs, 3)
	assert.Equal(t, llm.RoleUser, msgs[0].Role)
	assert.Equal(t, "q1", msgs[0].Text)
	assert.Equal(t, llm.RoleModel, msgs[1].Role)
	assert.Equal(t, "a1", msgs[1].Text)
	assert.Equal(t, llm.RoleUser, msgs[2].Role)
}

func TestHistoryToMessages_MergesToolExchangesIntoParentModel(t *testing.T) {
	h := []HistoryEntry{
		{Kind: EntryText, Role: RoleUser, Text: "find X"},
		{Kind: EntryText, Role: RoleModel, Text: "looking"},
		{Kind: EntryToolExchange,
			ToolCall:   &HistoryToolCall{CallID: "c0", Name: testToolRecall, Args: map[string]any{testArgQuery: "X"}},
			ToolResult: &HistoryToolResult{CallID: "c0", Result: map[string]any{testArgMemories: []any{"m1"}}},
		},
		{Kind: EntryToolExchange,
			ToolCall:   &HistoryToolCall{CallID: "c1", Name: testToolRecall, Args: map[string]any{testArgQuery: "Y"}},
			ToolResult: &HistoryToolResult{CallID: "c1", Result: map[string]any{testArgMemories: []any{"m2"}}},
		},
		{Kind: EntryText, Role: RoleModel, Text: "found it"},
	}
	msgs, err := HistoryToLLMMessages(h)
	require.NoError(t, err)
	require.Len(t, msgs, 4, "expected: User(q), Model(text+tools), User(tool results), Model(text)")
	assert.Equal(t, llm.RoleUser, msgs[0].Role)

	assert.Equal(t, llm.RoleModel, msgs[1].Role)
	assert.Equal(t, "looking", msgs[1].Text)
	require.Len(t, msgs[1].ToolCalls, 2, "both tool calls merged into parent model msg")
	assert.Equal(t, testToolRecall, msgs[1].ToolCalls[0].Name)

	assert.Equal(t, llm.RoleUser, msgs[2].Role, "tool results produce a User message")
	require.Len(t, msgs[2].ToolResults, 2)
	assert.Equal(t, testToolRecall, msgs[2].ToolResults[0].Name, "name must be threaded through for Vertex FunctionResponse")

	assert.Equal(t, llm.RoleModel, msgs[3].Role)
	assert.Equal(t, "found it", msgs[3].Text)
}

func TestHistoryToMessages_OrphanToolExchangeReturnsError(t *testing.T) {
	h := []HistoryEntry{
		{Kind: EntryToolExchange,
			ToolCall:   &HistoryToolCall{CallID: "x", Name: "t"},
			ToolResult: &HistoryToolResult{CallID: "x"},
		},
	}
	_, err := HistoryToLLMMessages(h)
	assert.Error(t, err)
}

func TestHistoryToMessages_NilToolCallReturnsError(t *testing.T) {
	h := []HistoryEntry{
		{Kind: EntryText, Role: RoleModel, Text: "t"},
		{Kind: EntryToolExchange, ToolCall: nil, ToolResult: &HistoryToolResult{}},
	}
	_, err := HistoryToLLMMessages(h)
	assert.Error(t, err)
}
