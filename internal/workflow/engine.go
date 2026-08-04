// Package workflow contains the generic YAML-interpreted Temporal workflow for Alfred.
package workflow

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"go.temporal.io/sdk/log"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/tphakala/alfred/internal/activity"
	"github.com/tphakala/alfred/internal/config"
	"github.com/tphakala/alfred/internal/memory"
	"github.com/tphakala/alfred/internal/tmpl"
)

// ApprovalSignalName is the Temporal signal name used to receive approval decisions.
const ApprovalSignalName = "approval"

const (
	activityTimeout  = 30 * time.Second
	maxRetryAttempts = 3
	retryBackoff     = 2.0
	maxRetryInterval = 30 * time.Second
	approvalTimeout  = 24 * time.Hour
)

// WorkflowInput is the input to the TicketWorkflow.
type WorkflowInput struct {
	TicketID       int64
	TicketData     map[string]any
	WorkflowConfig *config.WorkflowConfig
}

// ApprovalSignal carries a human approval decision for a workflow step.
type ApprovalSignal struct {
	Approved   bool
	ApproverID string
	Reason     string
}

// TicketWorkflow is the generic YAML-interpreted workflow for processing Autotask tickets.
// It is deterministic: all side effects are delegated to activities.
func TicketWorkflow(ctx workflow.Context, input WorkflowInput) error {
	logger := workflow.GetLogger(ctx)

	// Activity options: timeout, retry attempts, exponential backoff.
	ao := workflow.ActivityOptions{
		StartToCloseTimeout: activityTimeout,
		RetryPolicy: &temporal.RetryPolicy{
			MaximumAttempts:    maxRetryAttempts,
			InitialInterval:    1 * time.Second,
			BackoffCoefficient: retryBackoff,
			MaximumInterval:    maxRetryInterval,
		},
	}
	ctx = workflow.WithActivityOptions(ctx, ao)

	// Initialise mutable state from ticket data.
	state := make(map[string]any, len(input.TicketData))
	maps.Copy(state, input.TicketData)

	cfg := input.WorkflowConfig

	// --- Optional: recall relevant memories before processing. ---
	recallMemoriesIfNeeded(ctx, logger, cfg, input.TicketData, state)

	// --- Check approval once before entering the step loop. ---
	// Approval is workflow-level, not per-act-step, so we wait for it here.
	if cfg.Approval == "required" {
		if err := waitForApproval(ctx, input); err != nil {
			return fmt.Errorf("workflow: approval: %w", err)
		}
	}

	// --- Process steps. ---
	if err := runSteps(ctx, logger, cfg, input.TicketID, state); err != nil {
		return fmt.Errorf("execute steps: %w", err)
	}

	// --- Optional: retain a memory summary after processing. ---
	retainSummaryIfNeeded(ctx, logger, cfg, input.TicketID, state)

	return nil
}

// recallMemoriesIfNeeded optionally queries memory and stores results in state.
func recallMemoriesIfNeeded(ctx workflow.Context, logger log.Logger, cfg *config.WorkflowConfig, ticketData, state map[string]any) {
	if !cfg.Memory.RecallBefore {
		return
	}
	query := buildRecallQuery(ticketData)
	var memories []memory.MemoryItem
	if err := workflow.ExecuteActivity(ctx, "Recall", query).Get(ctx, &memories); err != nil {
		logger.Warn("memory recall failed, continuing without memories", "error", err)
	} else {
		state["memories"] = memories
	}
}

// retainSummaryIfNeeded optionally stores a state summary in memory after processing.
func retainSummaryIfNeeded(ctx workflow.Context, logger log.Logger, cfg *config.WorkflowConfig, ticketID int64, state map[string]any) {
	if !cfg.Memory.RetainAfter {
		return
	}
	summary := summarizeState(state)
	retainInput := activity.RetainInput{
		Content: summary,
		Tags:    []string{fmt.Sprintf("ticket-%d", ticketID)},
	}
	if err := workflow.ExecuteActivity(ctx, "Retain", retainInput).Get(ctx, nil); err != nil {
		logger.Warn("memory retain failed, continuing", "error", err)
	}
}

// runSteps executes each configured workflow step in order.
func runSteps(ctx workflow.Context, logger log.Logger, cfg *config.WorkflowConfig, ticketID int64, state map[string]any) error {
	for i, step := range cfg.Steps {
		if len(step) != 1 {
			return fmt.Errorf("step [%d]: must contain exactly one key, got %d", i, len(step))
		}
		for stepType, stepCfg := range step {
			if err := runStep(ctx, logger, cfg, ticketID, state, stepType, stepCfg); err != nil {
				return fmt.Errorf("step [%d] %s: %w", i, stepType, err)
			}
		}
	}
	return nil
}

// runStep dispatches a single step by type.
func runStep(ctx workflow.Context, logger log.Logger, cfg *config.WorkflowConfig, ticketID int64, state map[string]any, stepType string, stepCfg any) error {
	switch stepType {
	case "analyze", "draft_response":
		return runAnalyzeStep(ctx, cfg, state, stepCfg)
	case "act":
		return runActStep(ctx, ticketID, state, stepCfg)
	case "recall_similar":
		return runRecallSimilarStep(ctx, state, stepCfg)
	default:
		logger.Warn("unknown step type, skipping", "step_type", stepType)
	}
	return nil
}

// runAnalyzeStep renders the step prompt template, calls the LLM, and merges
// returned fields into state.
func runAnalyzeStep(ctx workflow.Context, cfg *config.WorkflowConfig, state map[string]any, stepCfg any) error {
	cfgMap, ok := stepCfg.(map[string]any)
	if !ok {
		return fmt.Errorf("analyze step config must be a map, got %T", stepCfg)
	}

	promptTmpl, ok := cfgMap["prompt"].(string)
	if !ok || promptTmpl == "" {
		return fmt.Errorf("analyze step requires a non-empty \"prompt\" string")
	}
	rendered, err := tmpl.RenderStrict(promptTmpl, state)
	if err != nil {
		return fmt.Errorf("render prompt: %w", err)
	}

	analyzeInput := activity.AnalyzeInput{
		Model:  cfg.Model,
		Prompt: rendered,
	}

	var output *activity.AnalyzeOutput
	if err := workflow.ExecuteActivity(ctx, "Analyze", analyzeInput).Get(ctx, &output); err != nil {
		return fmt.Errorf("LLM analyze: %w", err)
	}

	// Merge result fields into state.
	if output != nil {
		maps.Copy(state, output.Fields)
	}
	return nil
}

// runActStep executes a list of actions (add_note, set_field) against the ticket.
func runActStep(ctx workflow.Context, ticketID int64, state map[string]any, stepCfg any) error {
	actions, ok := stepCfg.([]any)
	if !ok {
		return fmt.Errorf("act step config must be a list, got %T", stepCfg)
	}

	for i, rawAction := range actions {
		actionMap, ok := rawAction.(map[string]any)
		if !ok {
			return fmt.Errorf("action [%d] must be a map, got %T", i, rawAction)
		}
		if len(actionMap) != 1 {
			return fmt.Errorf("action [%d]: must contain exactly one key, got %d", i, len(actionMap))
		}
		for actionType, actionVal := range actionMap {
			if err := runAction(ctx, ticketID, state, actionType, actionVal); err != nil {
				return fmt.Errorf("action [%d] %s: %w", i, actionType, err)
			}
		}
	}
	return nil
}

// runAction dispatches a single action by type.
func runAction(ctx workflow.Context, ticketID int64, state map[string]any, actionType string, actionVal any) error {
	switch actionType {
	case "add_note":
		return runAddNote(ctx, ticketID, state, actionVal)
	case "set_field":
		return runSetField(ctx, ticketID, state, actionVal)
	case "assign_queue":
		return runAssignQueue(ctx, ticketID, state, actionVal)
	default:
		workflow.GetLogger(ctx).Warn("unknown action type, skipping", "action_type", actionType)
	}
	return nil
}

// runAddNote renders a note template and posts it as a ticket note activity.
func runAddNote(ctx workflow.Context, ticketID int64, state map[string]any, actionVal any) error {
	noteTmpl, ok := actionVal.(string)
	if !ok || noteTmpl == "" {
		return fmt.Errorf("add_note requires a non-empty template string, got %T", actionVal)
	}
	content, err := tmpl.RenderStrict(noteTmpl, state)
	if err != nil {
		return fmt.Errorf("add_note render: %w", err)
	}
	noteInput := activity.AddNoteInput{
		TicketID: ticketID,
		Title:    "Alfred automated note",
		Content:  content,
	}
	if err := workflow.ExecuteActivity(ctx, "AddNote", noteInput).Get(ctx, nil); err != nil {
		return fmt.Errorf("add_note: %w", err)
	}
	return nil
}

// runSetField renders a value template and sets the named ticket field.
func runSetField(ctx workflow.Context, ticketID int64, state map[string]any, actionVal any) error {
	fieldCfg, ok := actionVal.(map[string]any)
	if !ok {
		return fmt.Errorf("set_field config must be a map, got %T", actionVal)
	}
	fieldName, ok := fieldCfg["name"].(string)
	if !ok || fieldName == "" {
		return fmt.Errorf("set_field requires a non-empty \"name\" string")
	}
	valueTmpl, ok := fieldCfg["value"].(string)
	if !ok || valueTmpl == "" {
		return fmt.Errorf("set_field %q requires a non-empty \"value\" string", fieldName)
	}
	rendered, err := tmpl.RenderStrict(valueTmpl, state)
	if err != nil {
		return fmt.Errorf("set_field render: %w", err)
	}
	fieldInput := activity.SetFieldInput{
		TicketID:  ticketID,
		FieldName: fieldName,
		Value:     rendered,
	}
	if err := workflow.ExecuteActivity(ctx, "SetField", fieldInput).Get(ctx, nil); err != nil {
		return fmt.Errorf("set_field %q: %w", fieldName, err)
	}
	return nil
}

// runAssignQueue renders a queue ID template and sets the ticket's queueID field.
func runAssignQueue(ctx workflow.Context, ticketID int64, state map[string]any, actionVal any) error {
	queueTmpl, ok := actionVal.(string)
	if !ok || queueTmpl == "" {
		return fmt.Errorf("assign_queue requires a non-empty template string, got %T", actionVal)
	}
	rendered, err := tmpl.RenderStrict(queueTmpl, state)
	if err != nil {
		return fmt.Errorf("assign_queue render: %w", err)
	}
	queueInput := activity.SetFieldInput{
		TicketID:  ticketID,
		FieldName: "queueID",
		Value:     rendered,
	}
	if err := workflow.ExecuteActivity(ctx, "SetField", queueInput).Get(ctx, nil); err != nil {
		return fmt.Errorf("assign_queue: %w", err)
	}
	return nil
}

// runRecallSimilarStep queries memory for similar past tickets and stores the
// results in state under the key "memories".
func runRecallSimilarStep(ctx workflow.Context, state map[string]any, stepCfg any) error {
	cfgMap, ok := stepCfg.(map[string]any)
	if !ok {
		return fmt.Errorf("recall_similar step config must be a map, got %T", stepCfg)
	}

	queryTmpl, ok := cfgMap["query"].(string)
	if !ok || queryTmpl == "" {
		return fmt.Errorf("recall_similar step requires a non-empty \"query\" string")
	}
	query, err := tmpl.RenderStrict(queryTmpl, state)
	if err != nil {
		return fmt.Errorf("recall_similar render query: %w", err)
	}

	var memories []memory.MemoryItem
	if err := workflow.ExecuteActivity(ctx, "Recall", query).Get(ctx, &memories); err != nil {
		return fmt.Errorf("recall_similar: %w", err)
	}

	state["memories"] = memories
	return nil
}

// waitForApproval posts an approval-request note and waits for an "approval"
// signal. It returns an error if the request is rejected or if no signal
// arrives within 24 hours.
func waitForApproval(ctx workflow.Context, input WorkflowInput) error {
	noteInput := activity.AddNoteInput{
		TicketID: input.TicketID,
		Title:    "Alfred approval request",
		Content:  "Alfred requires human approval before executing actions on this ticket.",
	}
	if err := workflow.ExecuteActivity(ctx, "AddNote", noteInput).Get(ctx, nil); err != nil {
		return fmt.Errorf("post approval request note: %w", err)
	}

	signalCh := workflow.GetSignalChannel(ctx, ApprovalSignalName)
	selector := workflow.NewSelector(ctx)

	var signal ApprovalSignal
	var received bool

	// timerCtx and cancelTimer allow us to cancel the timer goroutine once a
	// signal is received, preventing a resource leak.
	timerCtx, cancelTimer := workflow.WithCancel(ctx)
	defer cancelTimer()

	selector.AddReceive(signalCh, func(ch workflow.ReceiveChannel, more bool) {
		ch.Receive(ctx, &signal)
		received = true
		cancelTimer() // cancel the timer now that a signal has arrived
	})

	// Timer fires after approvalTimeout if no signal is received.
	timerFuture := workflow.NewTimer(timerCtx, approvalTimeout)
	selector.AddFuture(timerFuture, func(f workflow.Future) {
		// timeout — received stays false
	})

	selector.Select(ctx)

	if ctx.Err() != nil {
		return ctx.Err()
	}
	if !received {
		return fmt.Errorf("approval timed out after 24h")
	}
	if !signal.Approved {
		return fmt.Errorf("workflow rejected by %s: %s", signal.ApproverID, signal.Reason)
	}
	return nil
}

// buildRecallQuery constructs a memory query string from ticket Title and Description.
func buildRecallQuery(ticketData map[string]any) string {
	var parts []string
	if title, ok := ticketData["Title"].(string); ok && title != "" {
		parts = append(parts, title)
	}
	if desc, ok := ticketData["Description"].(string); ok && desc != "" {
		parts = append(parts, desc)
	}
	return strings.Join(parts, " ")
}

// summarizeState creates a "key=value" summary string from state, excluding
// reserved keys: Title, Description, memories.
func summarizeState(state map[string]any) string {
	excluded := map[string]bool{
		"Title":       true,
		"Description": true,
		"memories":    true,
	}

	keys := make([]string, 0, len(state))
	for k := range state {
		if !excluded[k] {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)

	pairs := make([]string, 0, len(keys))
	for _, k := range keys {
		pairs = append(pairs, fmt.Sprintf("%s=%v", k, state[k]))
	}
	return strings.Join(pairs, " ")
}
