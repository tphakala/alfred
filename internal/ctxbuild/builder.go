package ctxbuild

import (
	"context"
	"slices"
	"strings"

	"github.com/google/uuid"
)

// StoredMessage represents a message loaded from the database.
type StoredMessage struct {
	Sequence      int
	Role          string
	Content       string
	TokenEstimate int
}

// BuildParams configures a context build.
type BuildParams struct {
	SessionID    uuid.UUID
	TokenBudget  int
	Messages     []StoredMessage
	Memories     []string
	EpochSummary string
}

// BuiltMessage is a message ready for LLM consumption.
type BuiltMessage struct {
	Sequence      int
	Role          string
	Content       string
	TokenEstimate int
}

// LLMContext is the result of building context.
type LLMContext struct {
	Messages      []BuiltMessage
	TokensUsed    int
	TruncatedFrom int // total messages in full history
}

// ContextBuilder assembles LLM context from conversation history.
type ContextBuilder interface {
	Build(ctx context.Context, params *BuildParams) (*LLMContext, error)
}

// TieredContextBuilder prunes stale tool results, then fills newest-first.
type TieredContextBuilder struct {
	staleTurnThreshold int // tool results older than this many turns get pruned
}

// NewTieredContextBuilder creates a TieredContextBuilder with the given stale turn threshold.
func NewTieredContextBuilder(staleTurnThreshold int) *TieredContextBuilder {
	return &TieredContextBuilder{staleTurnThreshold: staleTurnThreshold}
}

// Message role constants matching the DB CHECK constraint on messages.role.
// Exported so workflow and other packages share a single source of truth.
const (
	RoleUser           = "user"
	RoleModel          = "model"
	RoleToolCall       = "tool_call"
	RoleToolResult     = "tool_result"
	RoleApprovalReq    = "approval_request"
	RoleApprovalResult = "approval_result"
	RoleContext        = "context"
	RoleError          = "error"
)

const (
	PrunedStubContent  = "[pruned: stale tool result]"
	prunedStubTokens   = 5
	charsPerToken      = 4
	prefixPartsInitCap = 2
	turnsPerPair       = 2
)

// EstimateTokens returns a rough token count for a string using the common
// heuristic of 4 characters per token.
func EstimateTokens(s string) int {
	return len(s) / charsPerToken
}

// Build assembles an LLMContext from the given params.
//
// Steps:
//  1. Build a "context" prefix message from EpochSummary and Memories (if present).
//  2. Prune stale tool results: replace content of old tool_result messages with a stub.
//  3. Fill from newest to oldest, stopping when the remaining budget is exceeded.
//  4. Return prefix (if any) followed by selected messages in sequence order.
func (b *TieredContextBuilder) Build(_ context.Context, params *BuildParams) (*LLMContext, error) {
	totalMessages := len(params.Messages)

	// --- Step 1: Build context prefix ---
	var prefixMsg *BuiltMessage
	prefixTokens := 0
	if params.EpochSummary != "" || len(params.Memories) > 0 {
		parts := make([]string, 0, prefixPartsInitCap+len(params.Memories))
		if params.EpochSummary != "" {
			parts = append(parts, params.EpochSummary)
		}
		if len(params.Memories) > 0 {
			parts = append(parts, strings.Join(params.Memories, "\n"))
		}
		content := strings.Join(parts, "\n")
		tokens := EstimateTokens(content)
		prefixMsg = &BuiltMessage{
			Role:          RoleContext,
			Content:       content,
			TokenEstimate: tokens,
		}
		prefixTokens = tokens
	}

	remainingBudget := max(params.TokenBudget-prefixTokens, 0)

	// --- Step 2: Find max sequence for staleness calculation ---
	maxSequence := 0
	for _, m := range params.Messages {
		if m.Sequence > maxSequence {
			maxSequence = m.Sequence
		}
	}

	// Prune stale tool results in-place (copy to avoid mutating input).
	pruned := make([]StoredMessage, len(params.Messages))
	copy(pruned, params.Messages)
	for i := range pruned {
		if pruned[i].Role != RoleToolResult && pruned[i].Role != RoleApprovalResult {
			continue
		}
		turnsAgo := (maxSequence - pruned[i].Sequence) / turnsPerPair
		if turnsAgo > b.staleTurnThreshold {
			pruned[i].Content = PrunedStubContent
			pruned[i].TokenEstimate = prunedStubTokens
		}
	}

	// --- Step 3: Fill from newest to oldest ---
	// Walk from last to first, accumulating until budget is exhausted.
	selected := make([]BuiltMessage, 0, len(pruned))
	tokensUsed := 0
	for _, m := range slices.Backward(pruned) {
		if tokensUsed+m.TokenEstimate > remainingBudget {
			break
		}
		selected = append(selected, BuiltMessage(m))
		tokensUsed += m.TokenEstimate
	}

	// Reverse selected so they are in ascending sequence order.
	for left, right := 0, len(selected)-1; left < right; left, right = left+1, right-1 {
		selected[left], selected[right] = selected[right], selected[left]
	}

	// --- Step 4: Build result ---
	messages := make([]BuiltMessage, 0, len(selected)+1)
	if prefixMsg != nil {
		messages = append(messages, *prefixMsg)
	}
	messages = append(messages, selected...)

	return &LLMContext{
		Messages:      messages,
		TokensUsed:    tokensUsed + prefixTokens,
		TruncatedFrom: totalMessages,
	}, nil
}
