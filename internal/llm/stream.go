package llm

// This file defines the streaming types for Client.Stream: Message, Role,
// StreamRequest, StreamChunk, and the StreamIterator interface. Concrete
// Stream implementations live in their respective provider files (vertex.go).

import (
	"errors"
)

// ErrEmptyMessages is returned by Client.Stream when StreamRequest.Messages
// is empty. Callers (e.g. the HTTP handler) can use errors.Is to classify
// this as a user input error and respond with HTTP 400.
var ErrEmptyMessages = errors.New("llm: Stream requires at least one message")

// ErrLastRoleNotUser is returned by Client.Stream when the last message in
// StreamRequest.Messages is a model-side (assistant) turn rather than a
// user-side turn: the model has nothing to respond to. Callers can use
// errors.Is to classify this as a user input error and respond with HTTP 400.
var ErrLastRoleNotUser = errors.New("llm: last message must be a user-side turn, not a model turn")

// isModelSideRole reports whether a message role represents a model (assistant)
// turn. A request must not end on one of these: the LLM needs a user-side turn
// to respond to. RoleToolCall and RoleApprovalReq are model-side (the model
// emitted them); every other role (user, tool_result, approval_result, context,
// error) is user-side for this check. These mirror the assistant-mapped roles
// in the provider role mappers (see mapRoleToOpenRouter and mapRoleToVertex).
func isModelSideRole(r Role) bool {
	switch r {
	case RoleModel, RoleToolCall, RoleApprovalReq:
		return true
	default:
		return false
	}
}

// validateStreamRequest checks the shared preconditions every provider's Stream
// enforces: at least one message, and a final message that is a user-side turn
// (not a model turn) so the model has something to respond to.
func validateStreamRequest(msgs []Message) error {
	if len(msgs) == 0 {
		return ErrEmptyMessages
	}
	if isModelSideRole(msgs[len(msgs)-1].Role) {
		return ErrLastRoleNotUser
	}
	return nil
}

// Role identifies the speaker of a conversation turn.
type Role string

const (
	// RoleUser marks a message sent by the end user.
	RoleUser Role = "user"
	// RoleModel marks a message produced by the LLM.
	RoleModel Role = "model"

	// The roles below originate in internal/ctxbuild and reach the providers as
	// flattened text messages. The llm package mirrors their string values here
	// (it must not import ctxbuild upward) so provider role mapping and the
	// user-side/model-side split have a single source of truth. These MUST match
	// the ctxbuild.Role* values byte for byte.
	RoleToolCall       Role = "tool_call"
	RoleToolResult     Role = "tool_result"
	RoleApprovalReq    Role = "approval_request"
	RoleApprovalResult Role = "approval_result"
	RoleContext        Role = "context"
	RoleError          Role = "error"
)

// Message is a single turn in a multi-turn conversation. The ToolCalls
// and ToolResults fields carry structured function-calling parts when the
// model invokes tools or the user provides tool results.
type Message struct {
	Role        Role
	Text        string
	ToolCalls   []ToolCallPart
	ToolResults []ToolResultPart
}

// ToolCallPart represents a single function call emitted by the model.
type ToolCallPart struct {
	CallID string
	Name   string
	Args   map[string]any
}

// ToolResultPart represents a single function result sent back to the model.
type ToolResultPart struct {
	CallID string
	Name   string
	Result any
	Error  string
}

// StreamRequest holds parameters for a streaming chat completion.
// Messages must contain at least one entry and the last entry must be a
// user-side turn (not a model/assistant turn). Temperature and MaxTokens are
// applied only when non-zero, matching the semantics of GenerateRequest.
type StreamRequest struct {
	Model             string
	Messages          []Message
	Temperature       float32
	MaxTokens         int32
	SystemInstruction string
	Tools             []*FunctionDeclaration
	ToolConfig        *ToolConfig
}

// StreamChunk is an incremental piece of a streaming response. Text is the
// new text fragment for this chunk (may be empty for metadata-only chunks).
// Tokens is non-nil only on the terminal usage chunk emitted immediately
// before the stream ends.
type StreamChunk struct {
	Text      string
	Tokens    *TokenUsage
	ToolCalls []ToolCallPart
}

// StreamIterator yields StreamChunks until the stream ends or errors.
//
// Concurrency contract: StreamIterator is NOT safe for concurrent use.
// Next and Close must be called from the same goroutine. This constraint
// comes from the underlying iter.Pull2 adapter (see the iter package docs:
// "It is an error to call next or stop from multiple goroutines
// simultaneously"). Cancellation therefore cannot be implemented by calling
// Close from a separate goroutine while another is blocked in Next;
// instead, cancel the context passed to Client.Stream, which propagates to
// the underlying HTTP call and causes Next to return with an error.
//
// Callers MUST call Close when done to release resources. Close is
// idempotent. After Next returns a non-nil error (including io.EOF), the
// iterator auto-closes; further calls to Next are safe and return io.EOF.
type StreamIterator interface {
	// Next returns the next StreamChunk. It returns io.EOF when the stream
	// has finished cleanly. Any other non-nil error also terminates the
	// stream and auto-closes the iterator; subsequent Next calls return
	// io.EOF.
	Next() (StreamChunk, error)
	// Close releases resources held by the iterator. Safe to call multiple
	// times. Must be called from the same goroutine as Next.
	Close() error
}
