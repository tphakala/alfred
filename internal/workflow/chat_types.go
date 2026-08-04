package workflow

import (
	"github.com/google/uuid"

	"github.com/tphakala/alfred/internal/llm"
)

// ValidatePromptRequest is the input for the ValidatePrompt activity.
type ValidatePromptRequest struct {
	SessionID      uuid.UUID
	Text           string
	HistorySummary string
}

// ValidatePromptResult is the output of the ValidatePrompt activity.
type ValidatePromptResult struct {
	Valid  bool
	Reason string
}

// LLMStreamRequest is the input for the LLMStream activity.
type LLMStreamRequest struct {
	SessionID         uuid.UUID
	TurnID            int
	Round             int
	Messages          []ContextMessage
	Tools             []*llm.FunctionDeclaration
	ToolConfig        *llm.ToolConfig
	SystemInstruction string
}

// LLMStreamResult is the output of the LLMStream activity.
type LLMStreamResult struct {
	Text      string
	ToolCalls []LLMToolCall
	Usage     *StreamTokenUsage
}

// LLMToolCall represents a tool call from the LLM response.
type LLMToolCall struct {
	CallID string
	Name   string
	Args   map[string]any
}

// StreamTokenUsage tracks token consumption from an LLM streaming call.
type StreamTokenUsage struct {
	PromptTokens   int32
	ResponseTokens int32
	TotalTokens    int32
}

// ExecToolRequest is the input for dynamic tool activity dispatch.
type ExecToolRequest struct {
	SessionID uuid.UUID      `json:"session_id"`
	TurnID    int            `json:"turn_id"`
	CallID    string         `json:"call_id"`
	Args      map[string]any `json:"args"`
}

// ExecToolResult is the output of dynamic tool activity dispatch.
type ExecToolResult struct {
	CallID string `json:"call_id"`
	Name   string `json:"name"`
	Result string `json:"result"`
	Error  string `json:"error,omitempty"`
}

// PersistRoundRequest is the input for the PersistRound activity.
type PersistRoundRequest struct {
	SessionID      uuid.UUID
	Round          int
	IdempotencyKey string
	Messages       []ActivityMessage
}

// PersistRejectionRequest is the input for the PersistAndEmitRejection activity.
type PersistRejectionRequest struct {
	SessionID uuid.UUID
	TurnID    int
	Reason    string
}

// AutoRecallRequest is the input for the AutoRecall activity.
type AutoRecallRequest struct {
	SessionID uuid.UUID
	Query     string
}

// AutoRecallResult is the output of the AutoRecall activity.
type AutoRecallResult struct {
	Memories string
}

// ChatStateResponse is the payload returned by the ChatStateQuery handler.
type ChatStateResponse struct {
	SessionID        uuid.UUID `json:"session_id"`
	Epoch            int       `json:"epoch"`
	TokensUsed       int       `json:"tokens_used"`
	TokenBudget      int       `json:"token_budget"`
	TurnID           int       `json:"turn_id"`
	CurrentRound     int       `json:"current_round"`
	PendingApprovals int       `json:"pending_approvals"`
	Status           string    `json:"status"`
}
