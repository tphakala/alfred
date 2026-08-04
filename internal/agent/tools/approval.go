package tools

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/tphakala/alfred/internal/llm"
)

const toolNameRequestApproval = "request_approval"

// argAction and argDescription are the canonical parameter names for the
// request_approval tool, extracted here to satisfy the goconst linter.
const (
	argAction      = "action"
	argDescription = "description"
)

// ApprovalResponse is the human decision sent back through the broker.
type ApprovalResponse struct {
	Approved bool
	Reason   string
}

// ApprovalBroker manages the lifecycle of pending approval requests.
type ApprovalBroker interface {
	WaitForApproval(ctx context.Context, callID string, emitEvent func()) (ApprovalResponse, error)
}

// ApprovalTool implements agent.Tool for requesting human approval before
// consequential actions.
type ApprovalTool struct {
	broker ApprovalBroker
	logger *slog.Logger
}

// NewApprovalTool builds an ApprovalTool with the given dependencies.
func NewApprovalTool(broker ApprovalBroker, logger *slog.Logger) *ApprovalTool {
	if broker == nil {
		panic("tools: NewApprovalTool requires a non-nil ApprovalBroker")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &ApprovalTool{broker: broker, logger: logger}
}

// Name implements agent.Tool.
func (t *ApprovalTool) Name() string { return toolNameRequestApproval }

// Declaration implements agent.Tool.
func (t *ApprovalTool) Declaration() *llm.FunctionDeclaration {
	return &llm.FunctionDeclaration{
		Name:        toolNameRequestApproval,
		Description: "Request human approval before performing a consequential action. The chat user will see the proposed action and can approve or reject it. Use this for destructive operations, ticket modifications, workflow triggers, or any action with side effects.",
		Parameters: &llm.Schema{
			Type: llm.TypeObject,
			Properties: map[string]*llm.Schema{
				argAction: {
					Type:        llm.TypeString,
					Description: "Short label for the action (e.g., 'Assign ticket', 'Close incident').",
				},
				argDescription: {
					Type:        llm.TypeString,
					Description: "Detailed description of what will happen if approved, including specific identifiers and values.",
				},
			},
			Required: []string{argAction, argDescription},
		},
	}
}

// Idempotent implements agent.Tool. ApprovalTool is intercepted by the
// workflow before dispatch and never executed as a dynamic activity; the
// value is true because requesting approval does not mutate external state.
func (t *ApprovalTool) Idempotent() bool { return true }

// Execute implements agent.Tool. It blocks until the broker returns a human
// decision or the context is cancelled.
func (t *ApprovalTool) Execute(ctx context.Context, args map[string]any) (any, error) {
	action, err := requireStringArg(args, argAction)
	if err != nil {
		return nil, fmt.Errorf(toolNameRequestApproval+": %w", err)
	}
	description, err := requireStringArg(args, argDescription)
	if err != nil {
		return nil, fmt.Errorf(toolNameRequestApproval+": %w", err)
	}

	callID, _ := args["_callId"].(string)
	emitEvent, _ := args["_emitEvent"].(func())
	if emitEvent == nil {
		emitEvent = func() {}
	}

	t.logger.DebugContext(ctx, toolNameRequestApproval+" waiting for approval",
		"action", action, "description", description, "callId", callID)

	resp, err := t.broker.WaitForApproval(ctx, callID, emitEvent)
	if err != nil {
		return nil, fmt.Errorf(toolNameRequestApproval+": %w", err)
	}

	result := map[string]any{"approved": resp.Approved}
	if resp.Reason != "" {
		result["reason"] = resp.Reason
	}

	t.logger.InfoContext(ctx, toolNameRequestApproval+" decision received",
		"action", action, "approved", resp.Approved, "callId", callID)

	return result, nil
}

// requireStringArg extracts a non-empty string value from args by key.
func requireStringArg(args map[string]any, key string) (string, error) {
	v, ok := args[key]
	if !ok {
		return "", fmt.Errorf("missing required argument %q", key)
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("%q must be a string, got %T", key, v)
	}
	if s == "" {
		return "", fmt.Errorf("%q must not be empty", key)
	}
	return s, nil
}

// PanicBroker satisfies ApprovalBroker for declaration-only registration.
// It panics if WaitForApproval is called, making the invariant explicit:
// when HaltOnApproval is true, the agent loop halts before executing the tool.
type PanicBroker struct{}

// NewPanicBroker returns a PanicBroker.
func NewPanicBroker() PanicBroker { return PanicBroker{} }

// WaitForApproval always panics — it should never be called.
func (PanicBroker) WaitForApproval(_ context.Context, _ string, _ func()) (ApprovalResponse, error) {
	panic("PanicBroker: WaitForApproval called in HaltOnApproval mode — this is a bug")
}
