package workflow

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/tphakala/alfred/internal/ctxbuild"
	"github.com/tphakala/alfred/internal/epoch"
	"github.com/tphakala/alfred/internal/llm"
)

const (
	UpdateSendMessage   = "send_message"
	UpdateRetryMessage  = "retry_message"
	UpdateApproveAction = "approve_action"

	// Query handler names. Cross-package callers (REST handlers, tests) must
	// reference these constants so the wire contract has one source of truth.
	ChatStateQuery     = "state"
	ChatSummaryQuery   = "summary"
	ChatApprovalsQuery = "approvals"

	ChatTaskQueue      = "alfred-chat"
	DefaultIdleTimeout = 30 * time.Minute
	DefaultTokenBudget = 100000

	// DefaultApprovalTimeout bounds how long a turn blocks waiting for a human
	// to resolve a request_approval before the request resolves as a timeout
	// (mirrors the TicketWorkflow approval gate's 24h bound). Without a bound,
	// an abandoned approval would hold the workflow open forever now that a
	// mid-turn idle-timer expiry re-arms instead of closing the workflow.
	DefaultApprovalTimeout = 24 * time.Hour

	// Approval status values.
	ApprovalStatusPending  = "pending"
	ApprovalStatusApproved = "approved"
	ApprovalStatusRejected = "rejected"
	ApprovalStatusTimeout  = "timeout"

	// Approve response status values.
	ApproveResponseOK       = "ok"
	ApproveResponseConflict = "conflict"

	maxAgentRounds     = 10
	llmActivityTimeout = 5 * time.Minute

	// Tool name for the approval interception tool.
	toolNameRequestApproval = "request_approval"

	// Workflow status values.
	statusIdle             = "idle"
	statusValidating       = "validating"
	statusStreaming        = "streaming"
	statusPersisting       = "persisting"
	statusAwaitingApproval = "awaiting_approval"

	// Retry policy defaults for workflow activities.
	defaultActivityTimeout     = 2 * time.Minute
	defaultRetryBackoff        = 2.0
	defaultRetryMaxAttempts    = 3
	defaultTokenThresholdRatio = 0.80
	defaultMaxEpochDuration    = 4 * time.Hour
)

// ChatWorkflowParams is the input to the ChatWorkflow.
type ChatWorkflowParams struct {
	SessionID         uuid.UUID
	Epoch             int
	EpochSummary      string
	Kind              string // "chat", "monitor", "ticket"
	AutoRecallEnabled bool
}

// ApprovalState tracks a pending approval within the workflow.
type ApprovalState struct {
	CallID      string
	Action      string
	Description string
	Status      string // ApprovalStatusPending, ApprovalStatusApproved, ApprovalStatusRejected, ApprovalStatusTimeout
	RequestedAt time.Time
	ResolvedAt  time.Time
	ResolvedBy  string // "chat_ui", "autotask", "slack", "timeout"
	Reason      string
}

// SendMessageRequest is the payload for the send_message update handler.
type SendMessageRequest struct {
	Text string
}

// SendMessageResponse is returned by the send_message update handler.
type SendMessageResponse struct {
	TurnID int
	Error  string
}

// RetryMessageRequest is the payload for the retry_message update handler.
type RetryMessageRequest struct {
	Text string
}

// RetryMessageResponse is returned by the retry_message update handler.
type RetryMessageResponse struct {
	TurnID int
	Error  string
}

// ApproveActionRequest is the payload for the approve_action update handler.
type ApproveActionRequest struct {
	CallID   string
	Approved bool
	Reason   string
}

// ApproveActionResponse is returned by the approve_action update handler.
type ApproveActionResponse struct {
	Status string // ApproveResponseOK, ApproveResponseConflict
	Error  string
}

// ActivityMessage represents a message produced by an activity for persistence.
type ActivityMessage struct {
	Role          string
	Content       string
	TokenEstimate int
	Metadata      map[string]any
}

// BuildContextResult is returned by the BuildContext activity.
type BuildContextResult struct {
	Messages   []ContextMessage
	TokensUsed int
}

// ContextMessage is a message prepared for LLM input.
type ContextMessage struct {
	Role    string
	Content string
}

// ExtractRequest is the input for the ExtractEpochSummary activity.
type ExtractRequest struct {
	SessionID uuid.UUID
	Messages  []ContextMessage
}

// ExtractResult is the output of the ExtractEpochSummary activity.
type ExtractResult struct {
	Summary        string
	ExtractedFacts string
}

// RetainHindsightRequest is the input for the RetainToHindsight activity.
type RetainHindsightRequest struct {
	SessionID      uuid.UUID
	ExtractedFacts string
	Epoch          int
}

// PersistEpochTransitionRequest is the input for the PersistEpochTransition activity.
type PersistEpochTransitionRequest struct {
	FromSessionID  uuid.UUID
	ToSessionID    uuid.UUID
	Trigger        string
	ExtractedFacts string
	Summary        string
}

// chatWorkflowState holds mutable state for a running ChatWorkflow instance.
type chatWorkflowState struct {
	params              ChatWorkflowParams
	turnID              int
	turnInProgress      bool // concurrency guard: prevents overlapping turns
	currentRound        int  // tracks round number for query handler
	status              string
	tokensUsed          int
	lastPreTurnSequence int // per-turn boundary for retry truncation; set by the send/retry handlers
	pendingApprovals    map[string]*ApprovalState
	resetCh             workflow.Channel // signals idle timer reset on activity
	turnDoneCh          workflow.Channel
	epochStartedAt      time.Time
	toolDeclarations    []*llm.FunctionDeclaration // cached from registry
	toolIdempotent      map[string]bool            // cached from registry; drives per-tool retry policy
	autoRecallEnabled   bool                       // from config
}

// allApprovalsResolved returns true when no pending approvals remain.
func (s *chatWorkflowState) allApprovalsResolved() bool {
	for _, a := range s.pendingApprovals {
		if a.Status == ApprovalStatusPending {
			return false
		}
	}
	return true
}

// ChatWorkflow is the Temporal workflow that orchestrates a single chat epoch.
// It registers update handlers for send_message and approve_action, then idles
// until DefaultIdleTimeout elapses without activity.
func ChatWorkflow(ctx workflow.Context, params ChatWorkflowParams) error { //nolint:gocognit,gocyclo // workflow orchestrator is inherently complex
	logger := workflow.GetLogger(ctx)

	state := &chatWorkflowState{
		params:            params,
		pendingApprovals:  make(map[string]*ApprovalState),
		resetCh:           workflow.NewBufferedChannel(ctx, 1),
		turnDoneCh:        workflow.NewBufferedChannel(ctx, 1),
		epochStartedAt:    workflow.Now(ctx),
		status:            statusIdle,
		autoRecallEnabled: params.AutoRecallEnabled,
	}

	// Activity options shared by all activities in the chat workflow.
	ao := workflow.ActivityOptions{
		StartToCloseTimeout: defaultActivityTimeout,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    time.Second,
			BackoffCoefficient: defaultRetryBackoff,
			MaximumAttempts:    defaultRetryMaxAttempts,
		},
	}
	actCtx := workflow.WithActivityOptions(ctx, ao)

	llmActOpts := workflow.ActivityOptions{
		StartToCloseTimeout: llmActivityTimeout,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    time.Second,
			BackoffCoefficient: defaultRetryBackoff,
			MaximumAttempts:    defaultRetryMaxAttempts,
		},
	}
	llmActCtx := workflow.WithActivityOptions(ctx, llmActOpts)

	// Register the update handlers before the workflow first blocks (the main
	// loop's selector), so an update that arrives immediately after start
	// finds a registered handler (Temporal buffers it until the handler runs)
	// rather than being rejected as an unknown update.
	// --- Update handler: send_message ---
	err := workflow.SetUpdateHandlerWithOptions(
		ctx,
		UpdateSendMessage,
		func(ctx workflow.Context, req SendMessageRequest) (SendMessageResponse, error) {
			if state.turnInProgress {
				return SendMessageResponse{Error: "a turn is already in progress"}, nil
			}
			state.turnInProgress = true
			defer func() { state.turnInProgress = false }()

			// Establish the retry-truncation boundary BEFORE the turn is
			// admitted: if this fetch fails, turnID stays untouched, so a
			// retry_message cannot run TruncateHistory against a boundary
			// this turn never set (with prior-epoch history in the session,
			// truncating to the zero boundary would wipe all of it).
			var maxSeq int
			if seqErr := workflow.ExecuteActivity(actCtx, "GetMaxSequence", params.SessionID).Get(ctx, &maxSeq); seqErr != nil {
				return SendMessageResponse{Error: seqErr.Error()}, nil //nolint:nilerr,gocritic // error encoded in response
			}
			state.lastPreTurnSequence = maxSeq

			state.turnID++
			turnID := state.turnID
			state.resetCh.Send(ctx, true)

			// Persist user message.
			persistErr := workflow.ExecuteActivity(actCtx, "Persist", params.SessionID, []ActivityMessage{
				{Role: ctxbuild.RoleUser, Content: req.Text, TokenEstimate: len(req.Text) / 4}, //nolint:mnd // rough token estimate: ~4 chars per token
			}).Get(ctx, nil)
			if persistErr != nil {
				return SendMessageResponse{Error: persistErr.Error()}, nil //nolint:nilerr,gocritic // error encoded in response
			}

			// Run agent turn (per-round activities).
			errMsg := runAgentTurnV2(ctx, actCtx, llmActCtx, state, params, turnID, req.Text)
			if errMsg != "" {
				return SendMessageResponse{Error: errMsg}, nil //nolint:nilerr,gocritic // error encoded in response
			}

			state.turnDoneCh.Send(ctx, true)
			return SendMessageResponse{TurnID: turnID}, nil
		},
		workflow.UpdateHandlerOptions{
			Validator: func(req SendMessageRequest) error {
				if req.Text == "" {
					return fmt.Errorf("message text is required")
				}
				return nil
			},
		},
	)
	if err != nil {
		return fmt.Errorf("register send_message handler: %w", err)
	}

	// --- Update handler: retry_message ---
	err = workflow.SetUpdateHandlerWithOptions(
		ctx,
		UpdateRetryMessage,
		func(ctx workflow.Context, req RetryMessageRequest) (RetryMessageResponse, error) {
			if state.turnInProgress {
				return RetryMessageResponse{Error: "a turn is already in progress"}, nil
			}
			state.turnInProgress = true
			defer func() { state.turnInProgress = false }()

			// 1. Truncate failed turn's messages.
			truncErr := workflow.ExecuteActivity(actCtx, "TruncateHistory",
				params.SessionID, state.lastPreTurnSequence,
			).Get(ctx, nil)
			if truncErr != nil {
				return RetryMessageResponse{Error: truncErr.Error()}, nil //nolint:nilerr,gocritic // error encoded in response
			}

			// 2. Clear stale approval state from the failed turn.
			state.pendingApprovals = make(map[string]*ApprovalState)

			// 3. Track new sequence boundary (post-truncation). As in
			// send_message, the fetch runs before the turn is admitted so a
			// failed fetch leaves the turn state untouched.
			var maxSeq int
			if seqErr := workflow.ExecuteActivity(actCtx, "GetMaxSequence", params.SessionID).Get(ctx, &maxSeq); seqErr != nil {
				return RetryMessageResponse{Error: seqErr.Error()}, nil //nolint:nilerr,gocritic // error encoded in response
			}
			state.lastPreTurnSequence = maxSeq

			state.turnID++
			turnID := state.turnID
			state.resetCh.Send(ctx, true)

			// 4. Persist user message.
			persistErr := workflow.ExecuteActivity(actCtx, "Persist", params.SessionID, []ActivityMessage{
				{Role: ctxbuild.RoleUser, Content: req.Text, TokenEstimate: len(req.Text) / 4}, //nolint:mnd // rough token estimate: ~4 chars per token
			}).Get(ctx, nil)
			if persistErr != nil {
				return RetryMessageResponse{Error: persistErr.Error()}, nil //nolint:nilerr,gocritic // error encoded in response
			}

			// 5. Run agent turn (per-round activities).
			errMsg := runAgentTurnV2(ctx, actCtx, llmActCtx, state, params, turnID, req.Text)
			if errMsg != "" {
				return RetryMessageResponse{Error: errMsg}, nil //nolint:nilerr,gocritic // error encoded in response
			}

			state.turnDoneCh.Send(ctx, true)
			return RetryMessageResponse{TurnID: turnID}, nil
		},
		workflow.UpdateHandlerOptions{
			Validator: func(req RetryMessageRequest) error {
				if req.Text == "" {
					return fmt.Errorf("message text is required")
				}
				if state.turnID == 0 {
					return fmt.Errorf("cannot retry: no message has been sent yet")
				}
				return nil
			},
		},
	)
	if err != nil {
		return fmt.Errorf("register retry_message handler: %w", err)
	}

	// --- Update handler: approve_action ---
	err = workflow.SetUpdateHandlerWithOptions(
		ctx,
		UpdateApproveAction,
		func(ctx workflow.Context, req ApproveActionRequest) (ApproveActionResponse, error) {
			approval, exists := state.pendingApprovals[req.CallID]
			if !exists || approval.Status != ApprovalStatusPending {
				return ApproveActionResponse{
					Status: ApproveResponseConflict,
					Error:  "approval not found or already resolved",
				}, nil
			}

			approval.Status = ApprovalStatusApproved
			if !req.Approved {
				approval.Status = ApprovalStatusRejected
			}
			approval.ResolvedAt = workflow.Now(ctx)
			approval.ResolvedBy = "chat_ui"
			approval.Reason = req.Reason

			return ApproveActionResponse{Status: ApproveResponseOK}, nil
		},
		workflow.UpdateHandlerOptions{
			Validator: func(req ApproveActionRequest) error {
				if req.CallID == "" {
					return fmt.Errorf("callID is required")
				}
				return nil
			},
		},
	)
	if err != nil {
		return fmt.Errorf("register approve_action handler: %w", err)
	}

	// --- Query handlers ---
	if err := workflow.SetQueryHandler(ctx, ChatStateQuery, func() (ChatStateResponse, error) {
		return ChatStateResponse{
			SessionID:        params.SessionID,
			Epoch:            params.Epoch,
			TokensUsed:       state.tokensUsed,
			TokenBudget:      DefaultTokenBudget,
			TurnID:           state.turnID,
			CurrentRound:     state.currentRound,
			PendingApprovals: countPending(state.pendingApprovals),
			Status:           state.status,
		}, nil
	}); err != nil {
		return fmt.Errorf("register state query handler: %w", err)
	}

	if err := workflow.SetQueryHandler(ctx, ChatSummaryQuery, func() (string, error) {
		return params.EpochSummary, nil
	}); err != nil {
		return fmt.Errorf("register summary query handler: %w", err)
	}

	if err := workflow.SetQueryHandler(ctx, ChatApprovalsQuery, func() ([]ApprovalState, error) {
		pending := make([]ApprovalState, 0, len(state.pendingApprovals))
		for _, a := range state.pendingApprovals {
			pending = append(pending, *a)
		}
		slices.SortFunc(pending, func(a, b ApprovalState) int {
			return a.RequestedAt.Compare(b.RequestedAt)
		})
		return pending, nil
	}); err != nil {
		return fmt.Errorf("register approvals query handler: %w", err)
	}

	// No init-time sequence lookup: lastPreTurnSequence is only read by the
	// retry handler, whose validator requires a prior turn, and both handlers
	// establish the boundary BEFORE admitting a turn (a turn that dies at its
	// boundary fetch leaves turnID untouched, so retry stays rejected). Token
	// counting needs no sequence scoping either: tokensUsed is in-memory per
	// run and resets on ContinueAsNew, so a fresh epoch starts with a fresh
	// budget and the epoch check (which only runs at turn end) cannot re-trip
	// with zero turns.
	//
	// Establishes a version-marker baseline at this sequence point (#65); no
	// branch, the absence of an init-time GetMaxSequence call (removed by
	// #85) is unconditional -- see the chat-tool-idempotency marker's
	// comment in Phase 1 for why.
	workflow.GetVersion(ctx, "chat-init-getmaxsequence", workflow.DefaultVersion, 1)

	// --- Main loop: idle timeout + epoch check ---
	for {
		timedOut := false
		checkEpoch := false
		timerCtx, timerCancel := workflow.WithCancel(ctx)
		timer := workflow.NewTimer(timerCtx, DefaultIdleTimeout)

		sel := workflow.NewSelector(ctx)
		sel.AddFuture(timer, func(f workflow.Future) {
			if f.Get(timerCtx, nil) == nil {
				timedOut = true
			}
		})
		sel.AddReceive(state.resetCh, func(ch workflow.ReceiveChannel, more bool) {
			var v bool
			ch.Receive(ctx, &v)
			timerCancel()
		})
		sel.AddReceive(state.turnDoneCh, func(ch workflow.ReceiveChannel, more bool) {
			var v bool
			ch.Receive(ctx, &v)
			checkEpoch = true
			timerCancel()
		})
		sel.Select(ctx)

		if ctx.Err() != nil {
			timerCancel()
			return ctx.Err()
		}
		if timedOut {
			timerCancel()
			// A timer expiry mid-turn is not idleness: the turn may be blocked
			// on a human approval (itself bounded by DefaultApprovalTimeout),
			// so re-arm the timer instead of completing the workflow out from
			// under the in-progress turn and its pending update handler.
			//
			// Establishes a version-marker baseline at this sequence point
			// (#65); no branch, this re-arm (added by the #84 fix) is
			// unconditional -- see the chat-tool-idempotency marker's
			// comment in Phase 1 for why.
			workflow.GetVersion(ctx, "chat-idle-timer-rearm", workflow.DefaultVersion, 1)
			if state.turnInProgress {
				continue
			}
			// The timer can expire in the same workflow task that completes a
			// turn; the selector prefers its first-added case (the timer), so
			// the turn-done token may still sit buffered here. Consume it and
			// run the epoch check instead of dropping it on the way out.
			var turnDone bool
			if state.turnDoneCh.ReceiveAsync(&turnDone) {
				checkEpoch = true
			} else {
				break
			}
		}
		timerCancel()

		if checkEpoch {
			now := workflow.Now(ctx)
			epochState := &epoch.EpochState{
				SessionID:      params.SessionID,
				Kind:           params.Kind,
				Epoch:          params.Epoch,
				TokensUsed:     state.tokensUsed,
				TokenBudget:    DefaultTokenBudget,
				EpochStartedAt: state.epochStartedAt,
				LastMessageAt:  now,
				Now:            now,
			}

			transition := checkEpochBoundary(epochState)
			if transition != nil {
				state.turnInProgress = true
				transitionErr := runEpochTransition(ctx, actCtx, llmActCtx, params, transition)
				if transitionErr != nil {
					if _, ok := errors.AsType[*workflow.ContinueAsNewError](transitionErr); !ok {
						logger.Error("epoch transition failed", "error", transitionErr)
					}
				}
				return transitionErr
			}
		}
	}

	logger.Info("ChatWorkflow completed", "sessionID", params.SessionID, "epoch", params.Epoch)
	return nil
}

// checkEpochBoundary evaluates epoch triggers deterministically (no I/O).
func checkEpochBoundary(state *epoch.EpochState) *epoch.EpochTransition {
	mgr := epoch.NewComposableEpochManager(epoch.ManagerConfig{
		TokenThreshold: defaultTokenThresholdRatio,
		MaxDuration:    defaultMaxEpochDuration,
	})
	return mgr.ShouldTransition(context.Background(), state)
}

// runEpochTransition runs the extract → retain → persist → ContinueAsNew sequence.
func runEpochTransition(
	ctx workflow.Context,
	actCtx workflow.Context,
	llmActCtx workflow.Context,
	params ChatWorkflowParams,
	transition *epoch.EpochTransition,
) error {
	logger := workflow.GetLogger(ctx)

	// a. Build context for extraction.
	var buildResult BuildContextResult
	buildErr := workflow.ExecuteActivity(actCtx, "BuildContext",
		params.SessionID, DefaultTokenBudget, params.EpochSummary,
	).Get(ctx, &buildResult)
	if buildErr != nil {
		return fmt.Errorf("epoch transition: build context: %w", buildErr)
	}

	// b. Extract epoch summary.
	extractReq := ExtractRequest{
		SessionID: params.SessionID,
		Messages:  buildResult.Messages,
	}
	var extractResult ExtractResult
	extractErr := workflow.ExecuteActivity(llmActCtx, "ExtractEpochSummary", extractReq).Get(ctx, &extractResult)
	if extractErr != nil {
		return fmt.Errorf("epoch transition: extract: %w", extractErr)
	}

	// c. Retain to Hindsight; errors logged, not propagated.
	// Temporal retries the activity up to 3 times before surfacing the error here.
	retainReq := RetainHindsightRequest{
		SessionID:      params.SessionID,
		ExtractedFacts: extractResult.ExtractedFacts,
		Epoch:          params.Epoch,
	}
	retainErr := workflow.ExecuteActivity(actCtx, "RetainToHindsight", retainReq).Get(ctx, nil)
	if retainErr != nil {
		logger.Warn("epoch transition: retain failed (non-fatal)", "error", retainErr)
	}

	// d. Persist epoch transition record.
	persistReq := PersistEpochTransitionRequest{
		FromSessionID:  params.SessionID,
		ToSessionID:    params.SessionID, // same session: ContinueAsNew reuses the session ID with incremented epoch
		Trigger:        transition.Trigger,
		ExtractedFacts: extractResult.ExtractedFacts,
		Summary:        extractResult.Summary,
	}
	persistErr := workflow.ExecuteActivity(actCtx, "PersistEpochTransition", persistReq).Get(ctx, nil)
	if persistErr != nil {
		return fmt.Errorf("epoch transition: persist: %w", persistErr)
	}

	// e. Update session row.
	newEpoch := params.Epoch + 1
	updateErr := workflow.ExecuteActivity(actCtx, "UpdateSessionEpochActivity",
		params.SessionID, newEpoch, extractResult.Summary,
	).Get(ctx, nil)
	if updateErr != nil {
		return fmt.Errorf("epoch transition: update session: %w", updateErr)
	}

	// f. Continue-As-New.
	return workflow.NewContinueAsNewError(ctx, ChatWorkflow, ChatWorkflowParams{
		SessionID:         params.SessionID,
		Epoch:             newEpoch,
		EpochSummary:      extractResult.Summary,
		Kind:              params.Kind,
		AutoRecallEnabled: params.AutoRecallEnabled,
	})
}

// buildSystemInstruction constructs the system instruction from workflow params.
// The configured system prompt (ChatActivities.SystemPrompt) is prepended by the
// LLMStream activity, so the workflow only handles the epoch summary prefix.
func buildSystemInstruction(params ChatWorkflowParams) string {
	si := ""
	if params.EpochSummary != "" {
		si = "Previous conversation context:\n" + params.EpochSummary + "\n\n"
	}
	return si
}

// toolMaxAttempts selects the per-tool activity retry budget: idempotent
// tools (side-effect-free, or safe to repeat) keep the default retry budget;
// a tool the registry does not report as idempotent gets exactly one
// attempt, since a retry could double a side effect. idempotent is
// state.toolIdempotent, populated once per turn from the ToolIdempotency
// activity; an unknown tool name (missing map entry) reads as
// non-idempotent, the safe default.
func toolMaxAttempts(idempotent map[string]bool, toolName string) int32 {
	if idempotent[toolName] {
		return defaultRetryMaxAttempts
	}
	return 1
}

// runAgentTurnV2 orchestrates a single turn using per-round activities instead
// of the monolithic RunAgentLoop activity. Each round issues an LLM call, then
// dispatches tool calls individually. This enables finer-grained Temporal
// visibility, retry policies per tool, and eliminates the need for heartbeat
// checkpointing within a single long-running activity.
func runAgentTurnV2( //nolint:gocognit,gocyclo // per-round orchestration is inherently complex
	ctx workflow.Context,
	actCtx workflow.Context,
	llmActCtx workflow.Context,
	state *chatWorkflowState,
	params ChatWorkflowParams,
	turnID int,
	userText string,
) string {
	logger := workflow.GetLogger(ctx)

	// --- Phase 0: Validate prompt (fail-open) ---
	state.status = statusValidating
	validateReq := ValidatePromptRequest{
		SessionID:      params.SessionID,
		Text:           userText,
		HistorySummary: params.EpochSummary,
	}
	var validateResult ValidatePromptResult
	if err := workflow.ExecuteActivity(llmActCtx, "ValidatePrompt", validateReq).Get(ctx, &validateResult); err != nil {
		logger.Warn("prompt validation failed, proceeding anyway", "error", err)
	} else if !validateResult.Valid {
		if err := workflow.ExecuteActivity(actCtx, "PersistAndEmitRejection", PersistRejectionRequest{
			SessionID: params.SessionID,
			TurnID:    turnID,
			Reason:    validateResult.Reason,
		}).Get(ctx, nil); err != nil {
			return fmt.Sprintf("persist rejection: %s", err)
		}
		state.status = statusIdle
		return ""
	}

	// --- Phase 1: Build initial context + resolve tool declarations ---
	if state.toolDeclarations == nil {
		var decls []*llm.FunctionDeclaration
		if err := workflow.ExecuteActivity(actCtx, "GetToolDeclarations", []string(nil)).Get(ctx, &decls); err != nil {
			return err.Error()
		}
		state.toolDeclarations = decls

		names := make([]string, len(decls))
		for i, d := range decls {
			names[i] = d.Name
		}

		// Establishes a version-marker baseline at this sequence point
		// (#65). This does NOT branch: every currently open workflow's
		// history already reflects this exact ToolIdempotency call (added
		// unconditionally in #55, and Temporal only went live on this host
		// after #55 shipped, so there is no surviving history shape without
		// it to protect against). The marker this call bakes into every
		// execution from this deploy onward exists so a FUTURE change to
		// this sequence point can branch against a known baseline (bump
		// maxSupported past 1) instead of repeating this same
		// retroactive-hardening problem.
		//
		// IMPORTANT for whoever adds that future branch: workflow.DefaultVersion
		// and v == 1 are semantically IDENTICAL for this changeID (and for
		// each of the other five #65 markers this comment is cross-referenced
		// from) because this marker was introduced with no accompanying
		// behavior change. A history replayed from BEFORE this marker existed
		// resolves to DefaultVersion; a history recorded AFTER it (by this
		// exact code) resolves to 1; both must take the CURRENT code path. A
		// future `if v == 1 {...} else if v == DefaultVersion {...}` that
		// treats them differently is a bug -- gate the NEW behavior behind
		// `v >= 2` and let both DefaultVersion and 1 fall through unchanged.
		workflow.GetVersion(ctx, "chat-tool-idempotency", workflow.DefaultVersion, 1)

		var idempotent map[string]bool
		if err := workflow.ExecuteActivity(actCtx, "ToolIdempotency", names).Get(ctx, &idempotent); err != nil {
			return err.Error()
		}
		state.toolIdempotent = idempotent
	}

	var ctxResult BuildContextResult
	if err := workflow.ExecuteActivity(actCtx, "BuildContext",
		params.SessionID, DefaultTokenBudget, params.EpochSummary,
	).Get(ctx, &ctxResult); err != nil {
		return err.Error()
	}

	systemInstruction := buildSystemInstruction(params)

	// --- Phase 1b: Auto-recall on first turn of new conversation ---
	if state.autoRecallEnabled && params.Epoch == 1 && state.turnID == 1 {
		var recallResult AutoRecallResult
		if err := workflow.ExecuteActivity(actCtx, "AutoRecall", AutoRecallRequest{
			SessionID: params.SessionID,
			Query:     userText,
		}).Get(ctx, &recallResult); err != nil {
			logger.Warn("auto-recall failed, proceeding without memories", "error", err)
		} else if recallResult.Memories != "" {
			systemInstruction += "\n\n" + recallResult.Memories
		}
	}

	// --- Phase 2: Round loop ---
roundLoop:
	for round := 1; round <= maxAgentRounds; round++ {
		state.currentRound = round

		// 2a. Determine tool config (force text-only on final round)
		var toolConfig *llm.ToolConfig
		roundSystemInstruction := systemInstruction
		if round == maxAgentRounds {
			toolConfig = &llm.ToolConfig{Mode: llm.ToolModeNone}
			roundSystemInstruction += "\n\nThis is your final response. Do not call any tools; provide the user with a direct text answer now."
		}

		// 2b. LLM streaming call
		state.status = statusStreaming
		llmReq := LLMStreamRequest{
			SessionID:         params.SessionID,
			TurnID:            turnID,
			Round:             round,
			Messages:          ctxResult.Messages,
			Tools:             state.toolDeclarations,
			ToolConfig:        toolConfig,
			SystemInstruction: roundSystemInstruction,
		}
		var llmResult LLMStreamResult
		if err := workflow.ExecuteActivity(llmActCtx, "LLMStream", llmReq).Get(ctx, &llmResult); err != nil {
			return err.Error()
		}

		// 2c. No tool calls → persist and done
		if len(llmResult.ToolCalls) == 0 {
			state.status = statusPersisting
			idempKey := fmt.Sprintf("%s-%d-%d", params.SessionID, turnID, round)
			if err := workflow.ExecuteActivity(actCtx, "PersistRound", PersistRoundRequest{
				SessionID:      params.SessionID,
				Round:          round,
				IdempotencyKey: idempKey,
				Messages: []ActivityMessage{{
					Role:          ctxbuild.RoleModel,
					Content:       llmResult.Text,
					TokenEstimate: estimateTokens(llmResult.Text),
				}},
			}).Get(ctx, nil); err != nil {
				return err.Error()
			}
			tokens := streamTokenCount(llmResult)
			state.tokensUsed += tokens
			if err := workflow.ExecuteActivity(actCtx, "EmitSSE", params.SessionID, "usage",
				map[string]any{"total": tokens}).Get(ctx, nil); err != nil {
				logger.Warn("EmitSSE failed", "event", "usage", "error", err)
			}
			if err := workflow.ExecuteActivity(actCtx, "EmitSSE", params.SessionID, "done",
				map[string]any{metaKeyTurnID: turnID}).Get(ctx, nil); err != nil {
				logger.Warn("EmitSSE failed", "event", "done", "error", err)
			}
			state.status = statusIdle
			return ""
		}

		// 2d. Round-cap defense (final round with tool calls despite MODE_NONE)
		if round == maxAgentRounds {
			text := llmResult.Text
			if text == "" {
				text = "I wasn't able to finish within the allowed number of steps. Please try rephrasing your request."
			}
			idempKey := fmt.Sprintf("%s-%d-%d", params.SessionID, turnID, round)
			if err := workflow.ExecuteActivity(actCtx, "PersistRound", PersistRoundRequest{
				SessionID:      params.SessionID,
				Round:          round,
				IdempotencyKey: idempKey,
				Messages:       []ActivityMessage{{Role: ctxbuild.RoleModel, Content: text, TokenEstimate: estimateTokens(text)}},
			}).Get(ctx, nil); err != nil {
				return err.Error()
			}
			if err := workflow.ExecuteActivity(actCtx, "EmitSSE", params.SessionID, "done",
				map[string]any{metaKeyTurnID: turnID}).Get(ctx, nil); err != nil {
				logger.Warn("EmitSSE failed", "event", "done", "error", err)
			}
			state.status = statusIdle
			return ""
		}

		// 2e. Execute tools via dynamic dispatch
		var toolMessages []ActivityMessage
		for _, call := range llmResult.ToolCalls {
			// --- Approval interception ---
			if call.Name == toolNameRequestApproval {
				state.status = statusAwaitingApproval

				// 1. Persist model text + tool results from earlier in this round.
				if len(toolMessages) > 0 || llmResult.Text != "" {
					idempKey := fmt.Sprintf("%s-%d-%d-pre-approval", params.SessionID, turnID, round)
					roundMsgs := buildRoundMessages(llmResult.Text, toolMessages)
					if err := workflow.ExecuteActivity(actCtx, "PersistRound", PersistRoundRequest{
						SessionID: params.SessionID, Round: round,
						IdempotencyKey: idempKey, Messages: roundMsgs,
					}).Get(ctx, nil); err != nil {
						return err.Error()
					}
				}

				// 2. Persist approval_request message (idempotent).
				action, _ := safeString(call.Args, metaKeyAction)
				description, _ := safeString(call.Args, metaKeyDescription)
				approvalReqKey := fmt.Sprintf("%s-%d-%d-approval-req", params.SessionID, turnID, round)
				if err := workflow.ExecuteActivity(actCtx, "PersistRound", PersistRoundRequest{
					SessionID:      params.SessionID,
					Round:          round,
					IdempotencyKey: approvalReqKey,
					Messages: []ActivityMessage{{
						Role:          ctxbuild.RoleApprovalReq,
						Content:       description,
						TokenEstimate: estimateTokens(description),
						Metadata: map[string]any{
							metaKeyCallID: call.CallID,
							metaKeyAction: action,
						},
					}},
				}).Get(ctx, nil); err != nil {
					return err.Error()
				}

				// 3. Register pending approval BEFORE SSE emission (prevents race).
				state.pendingApprovals[call.CallID] = &ApprovalState{
					CallID:      call.CallID,
					Action:      action,
					Description: description,
					Status:      ApprovalStatusPending,
					RequestedAt: workflow.Now(ctx),
				}

				// 4. Emit SSE approval_request + done events.
				if err := workflow.ExecuteActivity(actCtx, "EmitSSE", params.SessionID,
					"approval_request", map[string]any{
						metaKeyCallID: call.CallID, metaKeyTurnID: turnID,
						metaKeyName:   toolNameRequestApproval,
						metaKeyAction: action, metaKeyDescription: description,
					}).Get(ctx, nil); err != nil {
					logger.Warn("EmitSSE failed", "event", "approval_request", "error", err)
				}
				if err := workflow.ExecuteActivity(actCtx, "EmitSSE", params.SessionID,
					"done", map[string]any{metaKeyTurnID: turnID}).Get(ctx, nil); err != nil {
					logger.Warn("EmitSSE failed", "event", "done", "error", err)
				}

				// 5. Block until the approve_action update handler resolves it,
				// bounded by DefaultApprovalTimeout. On expiry every still
				// pending approval resolves as a timeout, which persists below
				// as approved:false so the LLM re-plans in the next round; a
				// later approve_action for it gets the normal conflict reply.
				//
				// Establishes a version-marker baseline at this sequence
				// point (#65); no branch, AwaitWithTimeout (added by the
				// #84 fix) is unconditional -- see the chat-tool-idempotency
				// marker's comment in Phase 1 for why.
				workflow.GetVersion(ctx, "chat-approval-timeout", workflow.DefaultVersion, 1)
				resolved, err := workflow.AwaitWithTimeout(ctx, DefaultApprovalTimeout, state.allApprovalsResolved)
				if err != nil {
					return err.Error()
				}
				// NOTE: !resolved is not authoritative on its own: the SDK can
				// report the timeout even when an approve_action landed in the
				// same workflow task. Only the per-entry Pending check below
				// decides; never key a side effect on !resolved alone. The map
				// iteration is determinism-safe only while its body emits no
				// commands (no activities or SSE inside this loop).
				if !resolved {
					for _, a := range state.pendingApprovals {
						if a.Status == ApprovalStatusPending {
							a.Status = ApprovalStatusTimeout
							a.ResolvedAt = workflow.Now(ctx)
							a.ResolvedBy = "timeout"
							a.Reason = "approval request timed out"
						}
					}
				}

				// 6. Persist approval_result message (idempotent).
				approval := state.pendingApprovals[call.CallID]
				approvalResKey := fmt.Sprintf("%s-%d-%d-approval-res", params.SessionID, turnID, round)
				if err := workflow.ExecuteActivity(actCtx, "PersistRound", PersistRoundRequest{
					SessionID:      params.SessionID,
					Round:          round,
					IdempotencyKey: approvalResKey,
					Messages: []ActivityMessage{{
						Role:          ctxbuild.RoleApprovalResult,
						Content:       marshalApprovalPayload(approval),
						TokenEstimate: estimateTokens(marshalApprovalPayload(approval)),
						Metadata:      map[string]any{metaKeyCallID: call.CallID},
					}},
				}).Get(ctx, nil); err != nil {
					return err.Error()
				}

				// 7. Rebuild context for next round.
				if err := workflow.ExecuteActivity(actCtx, "BuildContext",
					params.SessionID, DefaultTokenBudget, params.EpochSummary,
				).Get(ctx, &ctxResult); err != nil {
					return err.Error()
				}

				// Remaining tool calls dropped. LLM re-plans in next round.
				// Account for tokens consumed in this round before continuing.
				state.tokensUsed += streamTokenCount(llmResult) + toolTokenCount(toolMessages)
				continue roundLoop
			}

			// --- Empty/reserved tool name guard ---
			// dynamicToolActivityMethodName is reserved because
			// w.RegisterActivity(chatActivities)'s blanket struct scan also
			// registers DynamicToolActivity itself under its own Go method
			// name (see the registration comment in cmd/alfred/main.go); a
			// tool call literally named that would resolve to that reachable
			// but non-functional entry and fail with a confusing decode
			// error instead of a clean unknown-tool response (#116 follow-up).
			if call.Name == "" || call.Name == dynamicToolActivityMethodName {
				reason := "empty tool name from LLM"
				if call.Name != "" {
					reason = "reserved tool name from LLM: " + call.Name
				}
				toolMessages = append(toolMessages, buildToolMessages(call, &ExecToolResult{
					CallID: call.CallID, Name: call.Name,
					Result: "error: " + reason, Error: reason,
				})...)
				continue
			}

			// --- Dynamic tool dispatch ---
			state.status = fmt.Sprintf("executing_tool:%s", call.Name)

			toolReq := ExecToolRequest{
				SessionID: params.SessionID,
				TurnID:    turnID,
				CallID:    call.CallID,
				Args:      call.Args,
			}

			maxAttempts := toolMaxAttempts(state.toolIdempotent, call.Name)
			toolOpts := workflow.ActivityOptions{
				StartToCloseTimeout: defaultActivityTimeout,
				RetryPolicy: &temporal.RetryPolicy{
					InitialInterval:    time.Second,
					BackoffCoefficient: defaultRetryBackoff,
					MaximumAttempts:    maxAttempts,
				},
			}
			perToolCtx := workflow.WithActivityOptions(ctx, toolOpts)

			var toolResult ExecToolResult
			if err := workflow.ExecuteActivity(perToolCtx, call.Name, toolReq).Get(ctx, &toolResult); err != nil {
				toolResult = ExecToolResult{
					CallID: call.CallID,
					Name:   call.Name,
					Result: "error: " + err.Error(),
					Error:  err.Error(),
				}
			}
			toolMessages = append(toolMessages, buildToolMessages(call, &toolResult)...)
		}

		// 2f. Persist round (model text + all tool call/result pairs)
		state.status = statusPersisting
		idempKey := fmt.Sprintf("%s-%d-%d", params.SessionID, turnID, round)
		roundMessages := buildRoundMessages(llmResult.Text, toolMessages)
		if err := workflow.ExecuteActivity(actCtx, "PersistRound", PersistRoundRequest{
			SessionID:      params.SessionID,
			Round:          round,
			IdempotencyKey: idempKey,
			Messages:       roundMessages,
		}).Get(ctx, nil); err != nil {
			return err.Error()
		}

		// 2g. Update token count
		state.tokensUsed += streamTokenCount(llmResult) + toolTokenCount(toolMessages)

		// 2h. Rebuild context for next round
		if err := workflow.ExecuteActivity(actCtx, "BuildContext",
			params.SessionID, DefaultTokenBudget, params.EpochSummary,
		).Get(ctx, &ctxResult); err != nil {
			return err.Error()
		}
	}
	state.status = statusIdle
	return ""
}
