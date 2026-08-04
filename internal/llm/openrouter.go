package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"

	openrouter "github.com/revrost/go-openrouter"
)

// Compile-time interface satisfaction check.
var _ Client = &OpenRouterClient{}

// chatCompletionStream is an unexported interface covering the subset of
// openrouter.ChatCompletionStream that OpenRouterClient uses. Keeping it
// unexported lets tests substitute a fake without depending on the SDK.
type chatCompletionStream interface {
	Recv() (openrouter.ChatCompletionStreamResponse, error)
	Close()
}

// OpenRouterClient is a Client backed by the OpenRouter API.
type OpenRouterClient struct {
	client *openrouter.Client
	logger *slog.Logger
}

// NewOpenRouterClient creates an OpenRouterClient using the given API key.
// apiKey must be non-empty. If logger is nil a discard logger is used.
func NewOpenRouterClient(apiKey string, logger *slog.Logger) (*OpenRouterClient, error) {
	if apiKey == "" {
		return nil, fmt.Errorf("llm: openrouter api key must not be empty")
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &OpenRouterClient{
		client: openrouter.NewClient(apiKey),
		logger: logger,
	}, nil
}

// Close is a no-op; OpenRouter uses a stateless HTTP client.
func (o *OpenRouterClient) Close() error {
	return nil
}

// Generate sends a single prompt to the OpenRouter API and returns the
// generated content together with token usage metadata.
func (o *OpenRouterClient) Generate(ctx context.Context, req GenerateRequest) (*GenerateResponse, error) {
	msgs := []openrouter.ChatCompletionMessage{
		openrouter.UserMessage(req.Prompt),
	}

	orReq := openrouter.ChatCompletionRequest{
		Model:    req.Model,
		Messages: msgs,
		ResponseFormat: &openrouter.ChatCompletionResponseFormat{
			Type: openrouter.ChatCompletionResponseFormatTypeJSONObject,
		},
	}
	if req.Temperature != 0 {
		orReq.Temperature = req.Temperature
	}
	if req.MaxTokens != 0 {
		orReq.MaxTokens = int(req.MaxTokens)
	}

	resp, err := o.client.CreateChatCompletion(ctx, orReq)
	if err != nil {
		return nil, fmt.Errorf("llm: openrouter generate: %w", err)
	}

	var content string
	if len(resp.Choices) > 0 {
		content = resp.Choices[0].Message.Content.Text
	}

	var usage TokenUsage
	if resp.Usage != nil {
		usage = TokenUsage{
			PromptTokens:   int32(resp.Usage.PromptTokens),
			ResponseTokens: int32(resp.Usage.CompletionTokens),
			TotalTokens:    int32(resp.Usage.TotalTokens),
		}
	}

	o.logger.InfoContext(ctx, "llm token usage",
		"model", req.Model,
		"prompt_tokens", usage.PromptTokens,
		"response_tokens", usage.ResponseTokens,
		"total_tokens", usage.TotalTokens,
	)

	return &GenerateResponse{
		Content: content,
		Tokens:  usage,
	}, nil
}

// Stream opens a streaming chat completion against the OpenRouter API and
// returns a StreamIterator that yields StreamChunks as tokens arrive.
//
// OpenAI-compatible APIs fragment tool calls across multiple SSE chunks
// indexed by position. The returned iterator accumulates these fragments
// internally and emits complete tool calls only after the stream ends.
// Text chunks flow through immediately.
func (o *OpenRouterClient) Stream(ctx context.Context, req StreamRequest) (StreamIterator, error) { //nolint:gocritic // hugeParam: req must match Client interface signature.
	if err := validateStreamRequest(req.Messages); err != nil {
		return nil, err
	}

	msgs := messagesToOpenRouterMessages(req.Messages, req.SystemInstruction)

	orReq := openrouter.ChatCompletionRequest{
		Model:    req.Model,
		Messages: msgs,
		Stream:   true,
		StreamOptions: &openrouter.StreamOptions{
			IncludeUsage: true,
		},
	}
	if req.Temperature != 0 {
		orReq.Temperature = req.Temperature
	}
	if req.MaxTokens != 0 {
		orReq.MaxTokens = int(req.MaxTokens)
	}
	if len(req.Tools) > 0 {
		orReq.Tools = toOpenRouterTools(req.Tools)
		orReq.ToolChoice = toOpenRouterToolChoice(req.ToolConfig)
	}

	o.logger.InfoContext(ctx, "llm stream open",
		"model", req.Model,
		"messages", len(req.Messages),
	)

	stream, err := o.client.CreateChatCompletionStream(ctx, orReq)
	if err != nil {
		return nil, fmt.Errorf("llm: openrouter stream: %w", err)
	}

	return newOpenRouterStreamIterator(stream), nil
}

// maxToolCallIndex caps the tool-call index to prevent memory exhaustion from
// a malicious or buggy upstream that sends an enormous index value.
const maxToolCallIndex = 128

// accumulatedToolCall collects fragments of a single tool call spread across
// multiple SSE chunks. The Arguments field is built by concatenating JSON
// fragments as they arrive.
type accumulatedToolCall struct {
	ID        string
	Name      string
	Arguments string
}

// openRouterStreamIterator implements StreamIterator for OpenRouter streams.
// It works in two phases:
//
// Phase 1 (streaming): text content is emitted immediately; tool call
// fragments are accumulated silently by index; usage metadata is captured.
//
// Phase 2 (post-stream): accumulated tool calls and usage are emitted as
// separate chunks before returning io.EOF.
type openRouterStreamIterator struct {
	stream       chatCompletionStream
	accCalls     []*accumulatedToolCall
	usage        *TokenUsage
	streamDone   bool
	toolsEmitted bool
	usageEmitted bool
	closed       bool
}

// newOpenRouterStreamIterator wraps an OpenRouter stream in a StreamIterator.
func newOpenRouterStreamIterator(stream chatCompletionStream) *openRouterStreamIterator {
	return &openRouterStreamIterator{stream: stream}
}

// Next returns the next StreamChunk from the stream. See the type-level
// comment for the two-phase protocol.
func (it *openRouterStreamIterator) Next() (StreamChunk, error) { //nolint:gocognit,gocyclo // inherent complexity: two-phase state machine with tool-call accumulation
	if it.closed {
		return StreamChunk{}, io.EOF
	}

	// Phase 1: read from the underlying stream until EOF.
	for !it.streamDone {
		resp, err := it.stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) {
				it.streamDone = true
				break
			}
			// Real error: close and propagate.
			it.closed = true
			it.stream.Close()
			return StreamChunk{}, err
		}

		// Capture usage when present.
		if resp.Usage != nil {
			it.usage = &TokenUsage{
				PromptTokens:   int32(resp.Usage.PromptTokens),
				ResponseTokens: int32(resp.Usage.CompletionTokens),
				TotalTokens:    int32(resp.Usage.TotalTokens),
			}
		}

		var text string
		for i := range resp.Choices {
			// Accumulate text content.
			if resp.Choices[i].Delta.Content != "" {
				text += resp.Choices[i].Delta.Content
			}

			// Accumulate tool call fragments by index.
			for _, tc := range resp.Choices[i].Delta.ToolCalls {
				idx := 0
				if tc.Index != nil {
					idx = *tc.Index
				}
				if idx < 0 || idx >= maxToolCallIndex {
					it.closed = true
					it.stream.Close()
					return StreamChunk{}, fmt.Errorf("llm: tool call index %d out of range [0, %d)", idx, maxToolCallIndex)
				}
				for len(it.accCalls) <= idx {
					it.accCalls = append(it.accCalls, &accumulatedToolCall{})
				}
				acc := it.accCalls[idx]
				if tc.ID != "" {
					acc.ID = tc.ID
				}
				if tc.Function.Name != "" {
					acc.Name = tc.Function.Name
				}
				acc.Arguments += tc.Function.Arguments
			}
		}

		// Emit text immediately if we have any.
		if text != "" {
			return StreamChunk{Text: text}, nil
		}
		// Otherwise loop back to read the next chunk.
	}

	// Phase 2: emit accumulated data after the stream ends.

	// Emit accumulated tool calls (once).
	if !it.toolsEmitted {
		it.toolsEmitted = true
		if len(it.accCalls) > 0 {
			return StreamChunk{ToolCalls: it.finalizeToolCalls()}, nil
		}
	}

	// Emit usage (once).
	if !it.usageEmitted {
		it.usageEmitted = true
		if it.usage != nil {
			return StreamChunk{Tokens: it.usage}, nil
		}
	}

	// All done.
	it.closed = true
	it.stream.Close()
	return StreamChunk{}, io.EOF
}

// finalizeToolCalls converts accumulated tool call fragments into complete
// ToolCallPart values with parsed argument maps.
func (it *openRouterStreamIterator) finalizeToolCalls() []ToolCallPart {
	parts := make([]ToolCallPart, len(it.accCalls))
	for i, acc := range it.accCalls {
		var args map[string]any
		if err := json.Unmarshal([]byte(acc.Arguments), &args); err != nil {
			args = map[string]any{}
		}
		parts[i] = ToolCallPart{
			CallID: acc.ID,
			Name:   acc.Name,
			Args:   args,
		}
	}
	return parts
}

// Close releases resources held by the iterator. Idempotent.
func (it *openRouterStreamIterator) Close() error {
	if it.closed {
		return nil
	}
	it.closed = true
	it.stream.Close()
	return nil
}

// mapRoleToOpenRouter converts an llm Role to the OpenAI-compatible role
// string expected by OpenRouter.
func mapRoleToOpenRouter(r Role) string {
	switch r {
	case RoleModel, RoleToolCall, RoleApprovalReq:
		return openrouter.ChatMessageRoleAssistant
	case RoleContext:
		return openrouter.ChatMessageRoleSystem
	default:
		return openrouter.ChatMessageRoleUser
	}
}

// messagesToOpenRouterMessages converts provider-neutral messages to the
// OpenRouter SDK type. A non-empty systemInstruction is prepended as a system
// message. Tool calls and tool results embedded in messages are converted to
// the corresponding OpenRouter tool-call and tool-result messages.
func messagesToOpenRouterMessages(messages []Message, systemInstruction string) []openrouter.ChatCompletionMessage {
	var out []openrouter.ChatCompletionMessage

	if systemInstruction != "" {
		out = append(out, openrouter.SystemMessage(systemInstruction))
	}

	for _, m := range messages {
		// Messages with tool calls are assistant messages containing ToolCalls.
		if len(m.ToolCalls) > 0 {
			toolCalls := make([]openrouter.ToolCall, len(m.ToolCalls))
			for i, tc := range m.ToolCalls {
				argsJSON, _ := json.Marshal(tc.Args) //nolint:errchkjson // map[string]any always marshals
				toolCalls[i] = openrouter.ToolCall{
					ID:   tc.CallID,
					Type: openrouter.ToolTypeFunction,
					Function: openrouter.FunctionCall{
						Name:      tc.Name,
						Arguments: string(argsJSON),
					},
				}
			}
			msg := openrouter.ChatCompletionMessage{
				Role:      openrouter.ChatMessageRoleAssistant,
				ToolCalls: toolCalls,
			}
			if m.Text != "" {
				msg.Content = openrouter.Content{Text: m.Text}
			}
			out = append(out, msg)
			continue
		}

		// Messages with tool results become individual tool messages.
		if len(m.ToolResults) > 0 {
			for _, tr := range m.ToolResults {
				out = append(out, openrouter.ToolMessage(tr.CallID, openRouterToolResultString(tr)))
			}
			continue
		}

		// Plain text message.
		msg := openrouter.ChatCompletionMessage{
			Role:    mapRoleToOpenRouter(m.Role),
			Content: openrouter.Content{Text: m.Text},
		}
		out = append(out, msg)
	}

	return out
}

// openRouterToolResultString formats a ToolResultPart as a string for
// inclusion in an OpenRouter tool message.
func openRouterToolResultString(tr ToolResultPart) string {
	if tr.Error != "" {
		return tr.Error
	}
	if tr.Result == nil {
		return ""
	}
	switch v := tr.Result.(type) {
	case string:
		return v
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return strings.TrimSpace(fmt.Sprintf("%v", v))
		}
		return string(b)
	}
}

// toOpenRouterTools converts provider-neutral function declarations to
// OpenRouter tool definitions.
func toOpenRouterTools(decls []*FunctionDeclaration) []openrouter.Tool {
	if len(decls) == 0 {
		return nil
	}
	tools := make([]openrouter.Tool, len(decls))
	for i, d := range decls {
		tools[i] = openrouter.Tool{
			Type: openrouter.ToolTypeFunction,
			Function: &openrouter.FunctionDefinition{
				Name:        d.Name,
				Description: d.Description,
				Parameters:  d.Parameters,
			},
		}
	}
	return tools
}

// toOpenRouterToolChoice converts a provider-neutral ToolConfig to the
// OpenRouter tool_choice value (a string or nil).
func toOpenRouterToolChoice(tc *ToolConfig) any {
	if tc == nil {
		return nil
	}
	switch tc.Mode {
	case ToolModeAny:
		return "required"
	case ToolModeNone:
		return "none"
	case ToolModeAuto:
		return "auto"
	default:
		return "auto"
	}
}
