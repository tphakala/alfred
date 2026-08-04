package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/converter"

	"github.com/tphakala/alfred/internal/agent"
	"github.com/tphakala/alfred/internal/ctxbuild"
	"github.com/tphakala/alfred/internal/llm"
	"github.com/tphakala/alfred/internal/memory"
	"github.com/tphakala/alfred/internal/store"
)

// SessionEventPublisher is satisfied by server.SessionBrokerAdapter.
// Defined here with primitive parameters to avoid an import cycle between
// workflow → server → workflow.
type SessionEventPublisher interface {
	Publish(sessionID, eventType, data string)
}

// ChatActivities holds dependencies for chat workflow activities.
// Registered with the Temporal worker; each method is an activity.
type ChatActivities struct {
	Store             *store.Store
	CtxBuilder        ctxbuild.ContextBuilder
	SessionBroker     SessionEventPublisher
	Logger            *slog.Logger
	LLM               llm.Client
	ChatModel         string
	SystemPrompt      string
	Registry          *agent.Registry
	Memory            memory.MemoryClient
	AutoRecallEnabled bool
}

// metadata key constants used in tool call and result event payloads.
const (
	metaKeyCallID      = "callId"
	metaKeyName        = "name"
	metaKeyTurnID      = "turnId"
	metaKeyAction      = "action"
	metaKeyDescription = "description"
	metaKeyText        = "text"
	metaKeyArgs        = "args"
	unknownToolName    = "unknown_tool"
)

// BuildContext loads conversation history and builds an LLM context window.
func (a *ChatActivities) BuildContext(ctx context.Context, sessionID uuid.UUID, tokenBudget int, epochSummary string) (*BuildContextResult, error) {
	if a.Store == nil {
		return nil, fmt.Errorf("store is nil")
	}
	if a.CtxBuilder == nil {
		return nil, fmt.Errorf("context builder is nil")
	}

	msgs, err := a.Store.GetMessages(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("load messages for session %s: %w", sessionID, err)
	}

	stored := make([]ctxbuild.StoredMessage, len(msgs))
	for i := range msgs {
		stored[i] = ctxbuild.StoredMessage{
			Sequence:      msgs[i].Sequence,
			Role:          msgs[i].Role,
			Content:       msgs[i].Content,
			TokenEstimate: msgs[i].TokenEstimate,
		}
	}

	llmCtx, err := a.CtxBuilder.Build(ctx, &ctxbuild.BuildParams{
		SessionID:    sessionID,
		TokenBudget:  tokenBudget,
		Messages:     stored,
		EpochSummary: epochSummary,
	})
	if err != nil {
		return nil, fmt.Errorf("build context for session %s: %w", sessionID, err)
	}

	contextMsgs := make([]ContextMessage, len(llmCtx.Messages))
	for i, m := range llmCtx.Messages {
		contextMsgs[i] = ContextMessage{
			Role:    m.Role,
			Content: m.Content,
		}
	}

	return &BuildContextResult{
		Messages:   contextMsgs,
		TokensUsed: llmCtx.TokensUsed,
	}, nil
}

// convertActivityMessages converts ActivityMessage slices to store.Message slices.
// Shared by Persist and PersistRound to avoid duplicating the conversion logic.
func convertActivityMessages(msgs []ActivityMessage) ([]store.Message, error) {
	storeMessages := make([]store.Message, len(msgs))
	for i, msg := range msgs {
		var metadata json.RawMessage
		if len(msg.Metadata) > 0 {
			raw, err := json.Marshal(msg.Metadata)
			if err != nil {
				return nil, fmt.Errorf("marshal metadata for message %d: %w", i, err)
			}
			metadata = raw
		}
		storeMessages[i] = store.Message{
			ID:            uuid.New(),
			Role:          msg.Role,
			Content:       msg.Content,
			TokenEstimate: msg.TokenEstimate,
			Metadata:      metadata,
		}
	}
	return storeMessages, nil
}

// Persist appends activity messages to the store for a given session.
// Sequence numbers are allocated atomically inside a single transaction to
// prevent TOCTOU races between concurrent persist calls.
func (a *ChatActivities) Persist(ctx context.Context, sessionID uuid.UUID, messages []ActivityMessage) error {
	if a.Store == nil {
		return fmt.Errorf("store is nil: cannot persist messages for session %s", sessionID)
	}

	storeMessages, err := convertActivityMessages(messages)
	if err != nil {
		return fmt.Errorf("Persist: %w", err)
	}

	if err := a.Store.AppendMessagesAutoSeq(ctx, sessionID, storeMessages); err != nil {
		return fmt.Errorf("append messages for session %s: %w", sessionID, err)
	}

	return nil
}

// TruncateHistory removes all messages with sequence > afterSequence for the
// given session. Used by the retry_message workflow update to clean up partial
// messages from a failed turn before re-running it.
func (a *ChatActivities) TruncateHistory(ctx context.Context, sessionID uuid.UUID, afterSequence int) error {
	if a.Store == nil {
		return fmt.Errorf("store is nil: cannot truncate history for session %s", sessionID)
	}
	deleted, err := a.Store.DeleteMessagesAfterSequence(ctx, sessionID, afterSequence)
	if err != nil {
		return fmt.Errorf("truncate history for session %s after seq %d: %w", sessionID, afterSequence, err)
	}
	if a.Logger != nil {
		a.Logger.InfoContext(ctx, "truncated session history",
			"sessionId", sessionID.String(), "afterSequence", afterSequence, "deletedCount", deleted)
	}
	return nil
}

// GetMaxSequence returns the highest message sequence number for the given
// session, or 0 if the session has no messages.
func (a *ChatActivities) GetMaxSequence(ctx context.Context, sessionID uuid.UUID) (int, error) {
	if a.Store == nil {
		return 0, fmt.Errorf("store is nil: cannot get max sequence for session %s", sessionID)
	}
	next, err := a.Store.GetNextSequence(ctx, sessionID)
	if err != nil {
		return 0, fmt.Errorf("get max sequence for session %s: %w", sessionID, err)
	}
	return next - 1, nil
}

// StoredToHistory converts store.Message rows into agent.HistoryEntry slices.
// Tool call/result pairs are grouped into EntryToolExchange entries.
// Approval request/result pairs are treated as tool exchanges (request_approval).
// Messages with role "context" are skipped (epoch summary is injected via SystemInstruction).
func StoredToHistory(msgs []store.Message) ([]agent.HistoryEntry, error) { //nolint:gocognit,gocyclo // state machine with exhaustive role handling
	var history []agent.HistoryEntry
	i := 0
loop:
	for i < len(msgs) {
		msg := msgs[i]
		switch msg.Role {
		case ctxbuild.RoleUser:
			history = append(history, agent.HistoryEntry{
				Kind: agent.EntryText,
				Role: agent.RoleUser,
				Text: msg.Content,
			})
			i++
		case ctxbuild.RoleModel:
			history = append(history, agent.HistoryEntry{
				Kind: agent.EntryText,
				Role: agent.RoleModel,
				Text: msg.Content,
			})
			i++
		case ctxbuild.RoleToolCall:
			if i+1 >= len(msgs) || msgs[i+1].Role != ctxbuild.RoleToolResult {
				return nil, fmt.Errorf("storedToHistory: tool_call at index %d without following tool_result", i)
			}
			tc, err := extractToolCall(&msgs[i])
			if err != nil {
				return nil, fmt.Errorf("storedToHistory: index %d: %w", i, err)
			}
			tr, err := extractHistoryResult(&msgs[i+1], ctxbuild.RoleToolResult)
			if err != nil {
				return nil, fmt.Errorf("storedToHistory: index %d: %w", i+1, err)
			}
			history = append(history, agent.HistoryEntry{
				Kind:       agent.EntryToolExchange,
				ToolCall:   tc,
				ToolResult: tr,
			})
			i += 2
		case ctxbuild.RoleApprovalReq:
			if i+1 >= len(msgs) || msgs[i+1].Role != ctxbuild.RoleApprovalResult {
				// Dangling approval_request at end of history — approval is still
				// pending (e.g. recovery with in-flight approval). Treat as the end
				// of actionable history.
				break loop
			}
			tc, err := extractApprovalCall(&msgs[i])
			if err != nil {
				return nil, fmt.Errorf("storedToHistory: index %d: %w", i, err)
			}
			tr, err := extractHistoryResult(&msgs[i+1], ctxbuild.RoleApprovalResult)
			if err != nil {
				return nil, fmt.Errorf("storedToHistory: index %d: %w", i+1, err)
			}
			history = append(history, agent.HistoryEntry{
				Kind:       agent.EntryToolExchange,
				ToolCall:   tc,
				ToolResult: tr,
			})
			i += 2
		case ctxbuild.RoleToolResult, ctxbuild.RoleApprovalResult:
			// Orphaned result without preceding call — can happen after
			// TruncateHistory splits a call/result pair. Skip gracefully.
			i++
		case ctxbuild.RoleContext:
			i++
		default:
			return nil, fmt.Errorf("storedToHistory: unknown role %q at index %d", msg.Role, i)
		}
	}
	return history, nil
}

func extractToolCall(msg *store.Message) (*agent.HistoryToolCall, error) {
	var meta map[string]any
	if len(msg.Metadata) > 0 {
		if err := json.Unmarshal(msg.Metadata, &meta); err != nil {
			return nil, fmt.Errorf("unmarshal tool_call metadata: %w", err)
		}
	}
	callID, _ := meta[metaKeyCallID].(string)
	args, _ := meta["args"].(map[string]any)
	return &agent.HistoryToolCall{
		CallID: callID,
		Name:   msg.Content,
		Args:   args,
	}, nil
}

func extractHistoryResult(msg *store.Message, role string) (*agent.HistoryToolResult, error) {
	var meta map[string]any
	if len(msg.Metadata) > 0 {
		if err := json.Unmarshal(msg.Metadata, &meta); err != nil {
			return nil, fmt.Errorf("unmarshal %s metadata: %w", role, err)
		}
	}
	callID, _ := meta[metaKeyCallID].(string)
	return &agent.HistoryToolResult{
		CallID: callID,
		Result: msg.Content,
	}, nil
}

func extractApprovalCall(msg *store.Message) (*agent.HistoryToolCall, error) {
	var meta map[string]any
	if len(msg.Metadata) > 0 {
		if err := json.Unmarshal(msg.Metadata, &meta); err != nil {
			return nil, fmt.Errorf("unmarshal approval_request metadata: %w", err)
		}
	}
	callID, _ := meta[metaKeyCallID].(string)
	action, _ := meta[metaKeyAction].(string)
	return &agent.HistoryToolCall{
		CallID: callID,
		Name:   toolNameRequestApproval,
		Args:   map[string]any{metaKeyAction: action, metaKeyDescription: msg.Content},
	}, nil
}

// emitSessionEvent publishes an SSE event to session subscribers.
func (a *ChatActivities) emitSessionEvent(sessionID uuid.UUID, eventType string, data map[string]any) {
	publishSessionEvent(a.SessionBroker, a.Logger, sessionID, eventType, data)
}

// publishSessionEvent marshals data as JSON and publishes it to broker under
// eventType. Shared by ChatActivities.emitSessionEvent and
// CaseActivities.emitSessionEvent so the two struct types (which intentionally
// share no embedding) do not duplicate this logic. A nil broker is a no-op
// (SSE is always best-effort); a marshal failure is logged, not returned, for
// the same reason.
func publishSessionEvent(broker SessionEventPublisher, logger *slog.Logger, sessionID uuid.UUID, eventType string, data map[string]any) {
	if broker == nil {
		return
	}

	jsonBytes, err := json.Marshal(data)
	if err != nil {
		if logger != nil {
			logger.Warn("failed to marshal SSE event data",
				"event_type", eventType,
				"session_id", sessionID,
				"error", err,
			)
		}
		return
	}

	broker.Publish(sessionID.String(), eventType, string(jsonBytes))
}

// extractionPromptTemplate is the system prompt for epoch summary extraction.
const extractionPromptTemplate = `Analyze this conversation and extract:
1. Resolved: What was completed or answered
2. Pending: What remains open or unfinished
3. Decisions: Key decisions made during this epoch
4. Context: Critical context the next epoch needs

Respond ONLY with JSON: {"resolved":[],"pending":[],"decisions":[],"context":[]}`

// ExtractEpochSummary calls the LLM to produce a structured summary of the conversation.
func (a *ChatActivities) ExtractEpochSummary(ctx context.Context, req ExtractRequest) (*ExtractResult, error) {
	if a.LLM == nil {
		return nil, fmt.Errorf("ExtractEpochSummary: LLM client is nil")
	}

	var conversationBuf strings.Builder
	for _, m := range req.Messages {
		conversationBuf.WriteString(m.Role)
		conversationBuf.WriteString(": ")
		conversationBuf.WriteString(m.Content)
		conversationBuf.WriteString("\n")
	}

	prompt := extractionPromptTemplate + "\n\nConversation:\n" + conversationBuf.String()

	resp, err := a.LLM.Generate(ctx, llm.GenerateRequest{
		Model:  a.ChatModel,
		Prompt: prompt,
	})
	if err != nil {
		return nil, fmt.Errorf("ExtractEpochSummary: generate: %w", err)
	}

	extractedFacts := strings.TrimSpace(resp.Content)
	summary := buildEpochSummaryFromFacts(extractedFacts)

	return &ExtractResult{
		Summary:        summary,
		ExtractedFacts: extractedFacts,
	}, nil
}

// buildEpochSummaryFromFacts creates a concise text summary from the JSON extraction.
func buildEpochSummaryFromFacts(factsJSON string) string {
	var facts struct {
		Resolved  []string `json:"resolved"`
		Pending   []string `json:"pending"`
		Decisions []string `json:"decisions"`
		Context   []string `json:"context"`
	}
	if err := json.Unmarshal([]byte(factsJSON), &facts); err != nil {
		return factsJSON
	}

	var sb strings.Builder
	if len(facts.Resolved) > 0 {
		sb.WriteString("Resolved: ")
		sb.WriteString(strings.Join(facts.Resolved, "; "))
		sb.WriteString(". ")
	}
	if len(facts.Pending) > 0 {
		sb.WriteString("Pending: ")
		sb.WriteString(strings.Join(facts.Pending, "; "))
		sb.WriteString(". ")
	}
	if len(facts.Decisions) > 0 {
		sb.WriteString("Decisions: ")
		sb.WriteString(strings.Join(facts.Decisions, "; "))
		sb.WriteString(". ")
	}
	if len(facts.Context) > 0 {
		sb.WriteString("Context: ")
		sb.WriteString(strings.Join(facts.Context, "; "))
		sb.WriteString(".")
	}
	return strings.TrimSpace(sb.String())
}

// RetainToHindsight stores extracted epoch facts in Hindsight.
// Errors are logged but not propagated — Hindsight downtime must never block chat.
func (a *ChatActivities) RetainToHindsight(ctx context.Context, req RetainHindsightRequest) error { //nolint:gocritic // hugeParam: activity params are value-semantic for Temporal serialization
	if a.Memory == nil {
		if a.Logger != nil {
			a.Logger.Warn("RetainToHindsight: memory client is nil, skipping retain",
				"session_id", req.SessionID)
		}
		return nil
	}

	retainReq := memory.RetainRequest{
		Content: req.ExtractedFacts,
		Tags:    []string{"epoch_transition", fmt.Sprintf("session:%s", req.SessionID)},
		Metadata: map[string]string{
			"session_id": req.SessionID.String(),
			"epoch":      fmt.Sprintf("%d", req.Epoch),
		},
	}

	if err := a.Memory.Retain(ctx, retainReq); err != nil {
		if a.Logger != nil {
			a.Logger.Warn("RetainToHindsight: retain failed (non-fatal)",
				"session_id", req.SessionID,
				"epoch", req.Epoch,
				"error", err,
			)
		}
	}

	return nil
}

const (
	maxToolResultBytes = 4096
	truncationMarker   = "\n...(truncated)"
)

// formatToolResult converts an arbitrary tool result to a string,
// truncating to maxToolResultBytes on a valid rune boundary.
func formatToolResult(result any) string {
	if result == nil {
		return ""
	}
	if s, ok := result.(string); ok {
		return truncateString(s)
	}
	b, err := json.Marshal(result)
	if err != nil {
		return truncateString(fmt.Sprintf("%v", result))
	}
	return truncateString(string(b))
}

// truncateString returns s unchanged if it fits within maxToolResultBytes.
// Otherwise it truncates to maxToolResultBytes on a valid UTF-8 rune boundary
// and appends a truncation marker so consumers know the content was cut.
func truncateString(s string) string {
	if len(s) <= maxToolResultBytes {
		return s
	}
	cutAt := max(maxToolResultBytes-len(truncationMarker), 0)
	for cutAt > 0 && !utf8.RuneStart(s[cutAt]) {
		cutAt--
	}
	return s[:cutAt] + truncationMarker
}

// PersistEpochTransition creates an epoch_transition audit record in the store.
func (a *ChatActivities) PersistEpochTransition(ctx context.Context, req PersistEpochTransitionRequest) error { //nolint:gocritic // hugeParam: activity params are value-semantic for Temporal serialization
	if a.Store == nil {
		return fmt.Errorf("PersistEpochTransition: store is nil")
	}

	tr := &store.EpochTransition{
		ID:             uuid.New(),
		FromSession:    req.FromSessionID,
		ToSession:      req.ToSessionID,
		Trigger:        req.Trigger,
		ExtractedFacts: req.ExtractedFacts,
		Summary:        req.Summary,
	}

	if err := a.Store.CreateEpochTransition(ctx, tr); err != nil {
		return fmt.Errorf("PersistEpochTransition: %w", err)
	}

	return nil
}

// UpdateSessionEpochActivity updates the session's epoch number and summary in the store.
func (a *ChatActivities) UpdateSessionEpochActivity(ctx context.Context, sessionID uuid.UUID, epoch int, epochSummary string) error {
	if a.Store == nil {
		return fmt.Errorf("UpdateSessionEpochActivity: store is nil")
	}

	return a.Store.UpdateSessionEpoch(ctx, sessionID, epoch, epochSummary)
}

// ---------------------------------------------------------------------------
// Per-round activities for hybrid agent loop (v2)
// ---------------------------------------------------------------------------

// ValidatePrompt calls the LLM to check if the user's message is coherent.
// Fail-open on error: if the LLM response cannot be parsed, the message is
// assumed valid so the chat flow is never blocked by a validation glitch.
func (a *ChatActivities) ValidatePrompt(ctx context.Context, req ValidatePromptRequest) (*ValidatePromptResult, error) {
	if a.LLM == nil {
		return nil, fmt.Errorf("ValidatePrompt: LLM client is nil")
	}

	prompt := fmt.Sprintf(`Evaluate whether the following user message makes sense given the conversation context.
Context: %s

User message: %s

Respond ONLY with JSON: {"valid": true/false, "reason": "explanation if invalid"}`, req.HistorySummary, req.Text)

	resp, err := a.LLM.Generate(ctx, llm.GenerateRequest{
		Model:  a.ChatModel,
		Prompt: prompt,
	})
	if err != nil {
		return nil, fmt.Errorf("ValidatePrompt: generate: %w", err)
	}

	var result ValidatePromptResult
	if err := json.Unmarshal([]byte(strings.TrimSpace(resp.Content)), &result); err != nil { //nolint:musttag // ValidatePromptResult is internal, JSON tags unnecessary
		return &ValidatePromptResult{Valid: true}, nil //nolint:nilerr // fail-open: unparseable validation response → assume valid
	}
	return &result, nil
}

// LLMStream replaces the LLM-calling portion of RunAgentLoop for the hybrid
// loop. It streams via a.LLM.Stream(), emits SSE chunks, and records
// heartbeats. Tool call IDs are assigned with the format t{round}-c{index}.
func (a *ChatActivities) LLMStream(ctx context.Context, req LLMStreamRequest) (*LLMStreamResult, error) { //nolint:gocritic // hugeParam: activity params are value-semantic for Temporal serialization
	if a.LLM == nil {
		return nil, fmt.Errorf("LLMStream: LLM client is nil")
	}

	// Convert ContextMessage to llm.Message
	messages := make([]llm.Message, len(req.Messages))
	for i, m := range req.Messages {
		messages[i] = llm.Message{Role: llm.Role(m.Role), Text: m.Content}
	}

	// Prepend configured system prompt to the workflow-provided system instruction.
	// The workflow provides epoch summary + auto-recall memories; the activity
	// owns the base system prompt from ChatActivities.SystemPrompt.
	fullSystemInstruction := a.SystemPrompt
	if req.SystemInstruction != "" {
		if fullSystemInstruction != "" {
			fullSystemInstruction += "\n\n"
		}
		fullSystemInstruction += req.SystemInstruction
	}

	streamReq := llm.StreamRequest{
		Model:             a.ChatModel,
		Messages:          messages,
		Tools:             req.Tools,
		ToolConfig:        req.ToolConfig,
		SystemInstruction: fullSystemInstruction,
	}

	iter, err := a.LLM.Stream(ctx, streamReq)
	if err != nil {
		return nil, fmt.Errorf("LLMStream: stream start: %w", err)
	}
	defer func() { _ = iter.Close() }()

	text, toolCalls, usage, err := runLLMStreamLoop(ctx, iter, req.Round, req.TurnID, func(eventType string, data map[string]any) {
		a.emitSessionEvent(req.SessionID, eventType, data)
	})
	if err != nil {
		return nil, fmt.Errorf("LLMStream: %w", err)
	}

	return &LLMStreamResult{Text: text, ToolCalls: toolCalls, Usage: usage}, nil
}

// llmStreamEmitter forwards a streamed text chunk as an SSE-style "chunk"
// event. Shared by ChatActivities.LLMStream and CaseActivities.CaseLLMStream
// via runLLMStreamLoop.
type llmStreamEmitter func(eventType string, data map[string]any)

// runLLMStreamLoop drains iter to completion: accumulates text and tool
// calls, forwards non-empty text chunks to emit (nil-safe), tracks the final
// usage chunk, and heartbeats the activity every heartbeatThrottle. Tool call
// IDs are assigned in the "t{round}-c{index}" format once the stream ends.
//
// Extracted from ChatActivities.LLMStream so CaseActivities.CaseLLMStream does
// not duplicate this loop: the two callers differ only in how they build the
// outgoing llm.StreamRequest (chat prepends its configured system prompt and
// model; the case supervisor takes both entirely from the request) and in
// their result types, not in how they drain the response.
func runLLMStreamLoop(ctx context.Context, iter llm.StreamIterator, round, turnID int, emit llmStreamEmitter) (text string, toolCalls []LLMToolCall, usage *StreamTokenUsage, err error) {
	var (
		textBuf  strings.Builder
		rawCalls []llm.ToolCallPart
		lastHB   time.Time
	)

	for {
		chunk, chunkErr := iter.Next()
		if errors.Is(chunkErr, io.EOF) {
			break
		}
		if chunkErr != nil {
			return "", nil, nil, fmt.Errorf("iterator: %w", chunkErr)
		}

		if chunk.Text != "" {
			textBuf.WriteString(chunk.Text)
			if emit != nil {
				emit("chunk", map[string]any{metaKeyTurnID: turnID, metaKeyText: chunk.Text})
			}
		}
		rawCalls = append(rawCalls, chunk.ToolCalls...)
		if chunk.Tokens != nil {
			usage = &StreamTokenUsage{
				PromptTokens:   chunk.Tokens.PromptTokens,
				ResponseTokens: chunk.Tokens.ResponseTokens,
				TotalTokens:    chunk.Tokens.TotalTokens,
			}
		}

		// Heartbeat every 30s to keep activity alive
		if time.Since(lastHB) > heartbeatThrottle {
			activity.RecordHeartbeat(ctx, round)
			lastHB = time.Now()
		}
	}

	for i, call := range rawCalls {
		toolCalls = append(toolCalls, LLMToolCall{
			CallID: fmt.Sprintf("t%d-c%d", round, i),
			Name:   call.Name,
			Args:   call.Args,
		})
	}
	return textBuf.String(), toolCalls, usage, nil
}

// dynamicToolActivityMethodName is DynamicToolActivity's own Go method name,
// which w.RegisterActivity(chatActivities)'s blanket struct scan registers
// as an ordinary (reachable, not dead) activity alongside the intended
// RegisterDynamicActivity registration; a tool call scheduled under this
// literal name would resolve to that non-functional entry instead of the
// dynamic fallback. chat.go's tool-dispatch guard rejects it up front.
const dynamicToolActivityMethodName = "DynamicToolActivity"

// DynamicToolActivity is a dynamic activity handler that dispatches to tools
// by activity type name. The tool name is determined at runtime from the
// Temporal activity info, allowing a single handler to serve all registered
// tools without creating one activity per tool. Registered via
// worker.RegisterDynamicActivity, which requires this exact signature
// (converter.EncodedValues as the second parameter); the args are decoded
// manually below rather than bound by reflection like an ordinary activity.
func (a *ChatActivities) DynamicToolActivity(ctx context.Context, args converter.EncodedValues) (*ExecToolResult, error) {
	var req ExecToolRequest
	if err := args.Get(&req); err != nil {
		return nil, fmt.Errorf("decode dynamic tool args: %w", err)
	}

	toolName := activity.GetInfo(ctx).ActivityType.Name

	// Emit SSE tool_call
	a.emitSessionEvent(req.SessionID, "tool_call", map[string]any{
		metaKeyCallID: req.CallID,
		metaKeyTurnID: req.TurnID,
		metaKeyName:   toolName,
		metaKeyArgs:   req.Args,
	})

	// Execute via registry with panic recovery
	result, err := agent.SafeExecute(ctx, a.Registry, toolName, req.Args)

	resultStr := formatToolResult(result)
	errStr := ""
	if err != nil {
		errStr = err.Error()
		resultStr = "error: " + errStr
	}

	// Emit SSE tool_result
	a.emitSessionEvent(req.SessionID, "tool_result", map[string]any{
		metaKeyCallID: req.CallID,
		metaKeyTurnID: req.TurnID,
		metaKeyName:   toolName,
		"result":      resultStr,
	})

	return &ExecToolResult{
		CallID: req.CallID,
		Name:   toolName,
		Result: resultStr,
		Error:  errStr,
	}, nil
}

// PersistRound atomically persists one round's messages with idempotency keys
// to prevent duplicate writes on Temporal activity retries. Each message
// receives a key of the form "{idempotencyKey}-{index}".
func (a *ChatActivities) PersistRound(ctx context.Context, req PersistRoundRequest) error { //nolint:gocritic // hugeParam: activity params are value-semantic for Temporal serialization
	if a.Store == nil {
		return fmt.Errorf("PersistRound: store is nil")
	}

	storeMessages, err := convertActivityMessages(req.Messages)
	if err != nil {
		return fmt.Errorf("PersistRound: %w", err)
	}

	for i := range storeMessages {
		storeMessages[i].IdempotencyKey = fmt.Sprintf("%s-%d", req.IdempotencyKey, i)
	}

	if err := a.Store.AppendMessagesIdempotent(ctx, req.SessionID, storeMessages); err != nil {
		return fmt.Errorf("PersistRound: %w", err)
	}
	return nil
}

// AutoRecall queries Memory.Recall and formats matching memories as a system
// instruction prefix. Returns an empty result (no error) when Memory is nil.
func (a *ChatActivities) AutoRecall(ctx context.Context, req AutoRecallRequest) (*AutoRecallResult, error) { //nolint:gocritic // hugeParam: activity params are value-semantic for Temporal serialization
	if a.Memory == nil {
		return &AutoRecallResult{}, nil
	}

	resp, err := a.Memory.Recall(ctx, req.Query)
	if err != nil {
		return nil, fmt.Errorf("AutoRecall: %w", err)
	}
	if resp == nil || len(resp.Memories) == 0 {
		return &AutoRecallResult{}, nil
	}

	return &AutoRecallResult{Memories: agent.FormatRecallAsSystemInstruction(resp.Memories)}, nil
}

// EmitSSE is a thin activity for publishing SSE events that don't belong
// inside other activities (e.g. "done" events emitted by the workflow itself).
func (a *ChatActivities) EmitSSE(ctx context.Context, sessionID uuid.UUID, eventType string, data map[string]any) error {
	a.emitSessionEvent(sessionID, eventType, data)
	return nil
}

// PersistAndEmitRejection combines persistence and SSE for validation
// rejections. It stores the rejection reason as a model message and emits
// "chunk" + "done" SSE events so the frontend displays the rejection inline.
func (a *ChatActivities) PersistAndEmitRejection(ctx context.Context, req PersistRejectionRequest) error { //nolint:gocritic // hugeParam: activity params are value-semantic for Temporal serialization
	if err := a.Persist(ctx, req.SessionID, []ActivityMessage{{
		Role:          ctxbuild.RoleModel,
		Content:       req.Reason,
		TokenEstimate: estimateTokens(req.Reason),
	}}); err != nil {
		return fmt.Errorf("PersistAndEmitRejection: persist: %w", err)
	}
	a.emitSessionEvent(req.SessionID, "chunk", map[string]any{metaKeyTurnID: req.TurnID, metaKeyText: req.Reason})
	a.emitSessionEvent(req.SessionID, "done", map[string]any{metaKeyTurnID: req.TurnID})
	return nil
}

// GetToolDeclarations returns filtered tool declarations from the Registry.
// If allowedTools is empty, all declarations are returned.
func (a *ChatActivities) GetToolDeclarations(ctx context.Context, allowedTools []string) ([]*llm.FunctionDeclaration, error) {
	if a.Registry == nil {
		return nil, fmt.Errorf("GetToolDeclarations: registry is nil")
	}
	allDecls := a.Registry.Declarations()
	if len(allowedTools) == 0 {
		return allDecls, nil
	}

	allowed := make(map[string]bool, len(allowedTools))
	for _, name := range allowedTools {
		allowed[name] = true
	}

	var filtered []*llm.FunctionDeclaration
	for _, d := range allDecls {
		if allowed[d.Name] {
			filtered = append(filtered, d)
		}
	}
	return filtered, nil
}

// ToolIdempotency reports whether each named tool is idempotent, as recorded
// by the registry (agent.Tool.Idempotent). If names is empty, all registered
// tools are queried. Guards a nil registry by returning an empty map rather
// than an error: workflows and tests without a configured registry get a
// safe default, since a missing map entry already reads as false (the
// registry's own documented default for an unknown tool).
func (a *ChatActivities) ToolIdempotency(_ context.Context, names []string) (map[string]bool, error) {
	if a.Registry == nil {
		return map[string]bool{}, nil
	}
	if len(names) == 0 {
		for _, d := range a.Registry.Declarations() {
			names = append(names, d.Name)
		}
	}
	result := make(map[string]bool, len(names))
	for _, name := range names {
		result[name] = a.Registry.IsIdempotent(name)
	}
	return result, nil
}

// heartbeatThrottle is the minimum interval between throttled heartbeats
// within a single agent round to avoid Temporal overhead.
const heartbeatThrottle = 30 * time.Second
