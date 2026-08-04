package agent

import (
	"fmt"
	"strings"

	"github.com/tphakala/alfred/internal/llm"
	"github.com/tphakala/alfred/internal/memory"
)

// Role is user or model, matching llm.Role.
type Role string

const (
	RoleUser  Role = "user"
	RoleModel Role = "model"
)

// EntryKind distinguishes between text messages and tool exchanges in
// the flat history list.
type EntryKind int

const (
	// EntryText is a user or model text message.
	EntryText EntryKind = iota
	// EntryToolExchange is a completed tool call + result pair. Always
	// belongs to the immediately-preceding model EntryText.
	EntryToolExchange
)

// HistoryEntry is a single item in the flat history list.
type HistoryEntry struct {
	Kind EntryKind
	Role Role // used for EntryText only
	Text string

	// EntryToolExchange only.
	ToolCall   *HistoryToolCall
	ToolResult *HistoryToolResult
}

// HistoryToolCall is the model-side half of a historical tool exchange.
type HistoryToolCall struct {
	CallID string
	Name   string
	Args   map[string]any
}

// HistoryToolResult is the user-side half of a historical tool exchange.
type HistoryToolResult struct {
	CallID string
	Result any
	Error  string
}

// isNewConversation returns true when the history contains exactly one
// entry and it is a user text message. Used to gate auto-recall.
func IsNewConversation(history []HistoryEntry) bool {
	if len(history) != 1 {
		return false
	}
	e := history[0]
	return e.Kind == EntryText && e.Role == RoleUser
}

// firstUserText returns the text of the first user-role message in the
// history, or empty string if none.
func FirstUserText(history []HistoryEntry) string {
	for _, e := range history {
		if e.Kind == EntryText && e.Role == RoleUser {
			return e.Text
		}
	}
	return ""
}

// formatRecallAsSystemInstruction builds the text prepended to the Vertex
// SystemInstruction when auto-recall returns results. Keeps the format
// compact: no scores, no tags, bullet-listed content only.
func FormatRecallAsSystemInstruction(memories []memory.MemoryItem) string {
	if len(memories) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("Prior knowledge from memory (may be relevant to the user's question):\n\n")
	for _, m := range memories {
		sb.WriteString("- ")
		sb.WriteString(m.Content)
		sb.WriteString("\n")
	}
	return sb.String()
}

// historyToMessages converts a flat history into Vertex-compatible Messages.
// Tool exchanges are merged into the immediately-preceding model message's
// ToolCalls field, and a single user-role message containing all the
// FunctionResponse parts follows. Text-only user messages pass through.
//
// CRITICAL: a naive one-Message-per-HistoryEntry implementation would
// produce consecutive Model messages (Model-text, Model-toolCall, User-result)
// which Vertex rejects. This grouping algorithm prevents that.
func HistoryToLLMMessages(history []HistoryEntry) ([]llm.Message, error) {
	var messages []llm.Message
	i := 0
	for i < len(history) {
		entry := history[i]
		switch entry.Kind {
		case EntryText:
			if entry.Role == RoleUser {
				messages = append(messages, llm.Message{
					Role: llm.RoleUser,
					Text: entry.Text,
				})
				i++
				continue
			}
			// Model text: look ahead for tool exchanges in this turn.
			modelMsg := llm.Message{Role: llm.RoleModel, Text: entry.Text}
			toolResults := llm.Message{Role: llm.RoleUser}
			j := i + 1
			for j < len(history) && history[j].Kind == EntryToolExchange {
				te := history[j]
				if te.ToolCall == nil || te.ToolResult == nil {
					return nil, fmt.Errorf("agent: history entry %d: ToolExchange missing ToolCall or ToolResult", j)
				}
				modelMsg.ToolCalls = append(modelMsg.ToolCalls, llm.ToolCallPart{
					CallID: te.ToolCall.CallID,
					Name:   te.ToolCall.Name,
					Args:   te.ToolCall.Args,
				})
				toolResults.ToolResults = append(toolResults.ToolResults, llm.ToolResultPart{
					CallID: te.ToolResult.CallID,
					Name:   te.ToolCall.Name,
					Result: te.ToolResult.Result,
					Error:  te.ToolResult.Error,
				})
				j++
			}
			messages = append(messages, modelMsg)
			if len(toolResults.ToolResults) > 0 {
				messages = append(messages, toolResults)
			}
			i = j
		case EntryToolExchange:
			return nil, fmt.Errorf("agent: history entry %d: tool exchange without preceding model message", i)
		default:
			return nil, fmt.Errorf("agent: history entry %d: unknown kind %v", i, entry.Kind)
		}
	}
	return messages, nil
}
