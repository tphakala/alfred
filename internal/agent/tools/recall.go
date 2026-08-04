// Package tools contains concrete Tool implementations used by the agent
// package. Slice 1 ships recall_memories; future slices will add more.
package tools

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/tphakala/alfred/internal/llm"
	"github.com/tphakala/alfred/internal/memory"
)

// toolNameRecallMemories is the tool name used in function declarations.
const toolNameRecallMemories = "recall_memories"

// RecallTool wraps memory.MemoryClient.Recall as an agent Tool.
type RecallTool struct {
	memory memory.MemoryClient
	logger *slog.Logger
}

// NewRecallTool builds a RecallTool with the given dependencies.
func NewRecallTool(mem memory.MemoryClient, logger *slog.Logger) *RecallTool {
	if mem == nil {
		panic("tools: NewRecallTool requires a non-nil MemoryClient")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &RecallTool{memory: mem, logger: logger}
}

// Name implements agent.Tool.
func (t *RecallTool) Name() string { return toolNameRecallMemories }

// Declaration implements agent.Tool.
func (t *RecallTool) Declaration() *llm.FunctionDeclaration {
	return &llm.FunctionDeclaration{
		Name:        toolNameRecallMemories,
		Description: "Search the user's long-term memory for relevant prior context. Use this when the user asks about past decisions, ongoing projects, or prior conversations.",
		Parameters: &llm.Schema{
			Type: llm.TypeObject,
			Properties: map[string]*llm.Schema{
				argQuery: {
					Type:        llm.TypeString,
					Description: "Natural-language search query describing what to look up.",
				},
			},
			Required: []string{argQuery},
		},
	}
}

// argQuery is the key for the required "query" argument.
const argQuery = "query"

// Idempotent implements agent.Tool. RecallTool only reads memory and never
// mutates state, so repeated calls with the same query are safe to retry.
func (t *RecallTool) Idempotent() bool { return true }

// Execute implements agent.Tool. Argument validation happens here; the
// registry is a thin router.
func (t *RecallTool) Execute(ctx context.Context, args map[string]any) (any, error) {
	queryAny, ok := args[argQuery]
	if !ok {
		return nil, fmt.Errorf(toolNameRecallMemories+": missing required argument %q", argQuery)
	}
	query, ok := queryAny.(string)
	if !ok {
		return nil, fmt.Errorf(toolNameRecallMemories+": %q must be a string, got %T", argQuery, queryAny)
	}
	if query == "" {
		return nil, fmt.Errorf(toolNameRecallMemories+": %q must not be empty", argQuery)
	}

	t.logger.DebugContext(ctx, toolNameRecallMemories+" executing", argQuery, query)

	resp, err := t.memory.Recall(ctx, query)
	if err != nil {
		return nil, fmt.Errorf(toolNameRecallMemories+": %w", err)
	}
	if resp == nil {
		return map[string]any{"memories": []memory.MemoryItem{}}, nil
	}
	return map[string]any{"memories": resp.Memories}, nil
}
