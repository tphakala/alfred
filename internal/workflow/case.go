package workflow

import (
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/tphakala/alfred/internal/agentcfg"
	"github.com/tphakala/alfred/internal/ctxbuild"
	"github.com/tphakala/alfred/internal/store"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

// CaseInput is the input to CaseWorkflow: one candidate dispatched by a
// QueueMonitorWorkflow pass. Config is captured at dispatch time so a task
// config edit never affects an in-flight case. PromptDir is the deployment's
// prompt directory, threaded through unchanged so RenderCasePrompt and any
// spawned ExternalAgentWorkflow children can resolve prompt files the same
// way the parent queue_monitor task does.
type CaseInput struct {
	Task         string
	CandidateKey string
	Context      json.RawMessage
	Config       agentcfg.AgentConfig
	PromptDir    string
}

// CaseOutcome is the result of a case run.
type CaseOutcome struct {
	Status string `json:"status"`
}

const (
	loopActivityTimeout    = 30 * time.Second
	loopActivityMaxAttempt = 3

	// defaultAgentLoopMaxRounds bounds the shared ReAct loop when a deployment
	// leaves MaxRounds unset (Supervisor.MaxRounds or Native.MaxRounds, zero
	// value); both runAgentLoop parameterizations fall back to it.
	defaultAgentLoopMaxRounds = 10

	// declaredToolTimeout bounds a single RunDeclaredCommand attempt.
	declaredToolTimeout = 2 * time.Minute

	// Retry attempt counts for declared command tools: a non-idempotent tool
	// runs at most once (a retry could double a side effect the deployment
	// never promised was safe to repeat); an idempotent tool may retry up to
	// declaredToolIdempotentAttempts times.
	declaredToolSingleAttempt      = 1
	declaredToolIdempotentAttempts = 3

	// agentLoopTurnID is the constant CaseLLMStreamRequest.TurnID for every
	// round of the shared agent loop: unlike chat, the case and native loops
	// have no concept of multiple user turns, only rounds, so the turn id
	// never varies.
	agentLoopTurnID = 1

	// messagesPerToolCall is how many ActivityMessage entries
	// buildToolMessages always returns for one tool call: a tool_call
	// message and its paired tool_result message.
	messagesPerToolCall = 2

	// defaultSupervisorSystemInstruction is used when a deployment's
	// Supervisor.Prompt.Base is empty, so the loop still runs with some
	// system instruction instead of none.
	defaultSupervisorSystemInstruction = "You are the case supervisor. Work the task using the " +
		"tools available to you, then call record_outcome with a terminal status once you have " +
		"reached a conclusion."
)

// CaseWorkflow is the Level 1 case supervisor: an autonomous LLM ReAct loop,
// one per work item. Each round it streams one LLM turn, executes whatever
// tools the LLM called (spawning sub-agent children, running declared
// commands), persists the round, and checks whether the LLM finalized the
// case via record_outcome. Guards on wall-clock duration, cumulative cost,
// and round count force the case to a needs_attention outcome if none of
// them trip first.
func CaseWorkflow(ctx workflow.Context, in CaseInput) (CaseOutcome, error) { //nolint:gocognit,gocyclo,gocritic // per-round orchestration is inherently complex, mirrors runAgentTurnV2; hugeParam: workflow inputs are value-semantic for Temporal serialization
	sup := in.Config.Supervisor
	maxRounds := sup.MaxRounds
	if maxRounds <= 0 {
		maxRounds = defaultAgentLoopMaxRounds
	}
	declaredByName := make(map[string]agentcfg.DeclaredTool, len(sup.Tools))
	for _, t := range sup.Tools {
		declaredByName[t.Name] = t
	}

	var sessionID uuid.UUID
	encoded := workflow.SideEffect(ctx, func(_ workflow.Context) any {
		return uuid.New()
	})
	if err := encoded.Get(&sessionID); err != nil {
		return CaseOutcome{}, err
	}

	wfInfo := workflow.GetInfo(ctx)
	claimOpts := shortActivityCtx(ctx)
	if err := workflow.ExecuteActivity(claimOpts, "ClaimCase", ClaimCaseArgs{
		Task:         in.Task,
		CandidateKey: in.CandidateKey,
		SessionID:    sessionID,
		WorkflowID:   wfInfo.WorkflowExecution.ID,
		RunID:        wfInfo.WorkflowExecution.RunID,
	}).Get(ctx, nil); err != nil {
		return CaseOutcome{}, err
	}

	actCtx := defaultLoopActivityCtx(ctx)

	systemInstruction := defaultSupervisorSystemInstruction
	if sup.Prompt.Base != "" {
		vars := map[string]any{
			"task":          in.Task,
			"candidate_key": in.CandidateKey,
		}
		if err := workflow.ExecuteActivity(actCtx, "RenderCasePrompt",
			in.PromptDir, sup.Prompt, vars).Get(ctx, &systemInstruction); err != nil {
			return CaseOutcome{}, err
		}
	}

	initialContext := string(in.Context)
	if initialContext == "" {
		initialContext = "Begin working the task."
	}
	if err := workflow.ExecuteActivity(actCtx, "PersistRound", PersistRoundRequest{
		SessionID:      sessionID,
		IdempotencyKey: fmt.Sprintf("case-%s-init", sessionID),
		Messages: []ActivityMessage{{
			Role:          ctxbuild.RoleUser,
			Content:       initialContext,
			TokenEstimate: estimateTokens(initialContext),
		}},
	}).Get(ctx, nil); err != nil {
		return CaseOutcome{}, err
	}

	toolDecls := supervisorToolDeclarations(sup)

	idemPrefix := fmt.Sprintf("case-%s", sessionID)

	handleSupervisorTools := func(loopCtx workflow.Context, round int, text string, calls []LLMToolCall) (float64, *loopStop, error) {
		terminal, spawns, declared, unknown := classifyToolCalls(calls, supervisorTerminalNames, agentcfg.ReservedToolSpawnAgent, declaredByName)

		var toolMessages []ActivityMessage //nolint:prealloc // size spans several append sources; not worth a speculative cap
		spawnMsgs, spawnCost := runSpawnAgents(loopCtx, wfInfo, in, sup, sessionID, round, spawns)
		toolMessages = append(toolMessages, spawnMsgs...)
		toolMessages = append(toolMessages, runDeclaredTools(loopCtx, declaredByName, declared)...)
		toolMessages = append(toolMessages, runUnknownToolCalls(unknown, supervisorTerminalNames)...)

		if err := persistLoopRound(loopCtx, actCtx, sessionID, round, idemPrefix, loopRoundMessages(text, len(calls) > 0, toolMessages)); err != nil {
			return spawnCost, nil, err
		}
		if terminal == nil {
			return spawnCost, nil, nil
		}
		status, _ := safeString(terminal.Args, argRecordStatus)
		if !isValidRecordOutcomeStatus(status) {
			if err := persistInvalidRecordOutcome(loopCtx, actCtx, sessionID, round, idemPrefix, *terminal, status); err != nil {
				return spawnCost, nil, err
			}
			return spawnCost, nil, nil
		}
		if err := persistAcceptedTerminal(loopCtx, actCtx, sessionID, round, idemPrefix, *terminal, fmt.Sprintf("recorded outcome: %s", status)); err != nil {
			return spawnCost, nil, err
		}
		return spawnCost, &loopStop{Terminal: terminal}, nil
	}

	outcome, err := runAgentLoop(ctx, agentLoopSpec{
		SessionID:         sessionID,
		Model:             sup.Model,
		SystemInstruction: systemInstruction,
		ToolDecls:         toolDecls,
		MaxRounds:         maxRounds,
		MaxDuration:       sup.MaxDuration,
		CostCapUSD:        sup.CostCapUSD,
		HandleTools:       handleSupervisorTools,
	})
	if err != nil {
		return CaseOutcome{}, err
	}

	// Surface the case's total metered spend (own LLM rounds plus all spawned
	// sub-agent cost) once at finalize. GetLogger suppresses this on replay, so
	// it is determinism-safe. The task_state ledger records only the terminal
	// status/outcome, so this log is where an operator sees the dollar figure.
	workflow.GetLogger(ctx).Info("case supervisor loop finished",
		"task", in.Task, "candidate_key", in.CandidateKey, "cost_usd", outcome.CostUSD)

	if outcome.Terminal != nil {
		// Only statuses isValidRecordOutcomeStatus accepted produce a
		// terminal here, so the re-extraction cannot yield an invalid status.
		status, _ := safeString(outcome.Terminal.Args, argRecordStatus)
		payload, perr := recordOutcomePayload(outcome.Terminal.Args)
		if perr != nil {
			return CaseOutcome{}, perr
		}
		return finalizeCase(ctx, in, status, payload)
	}
	return finalizeNeedsAttention(ctx, in, outcome.GuardTrip)
}

// supervisorTerminalNames is the Case Supervisor's single terminal tool.
var supervisorTerminalNames = []string{agentcfg.ReservedToolRecordOutcome}

// pendingSpawn pairs a spawn_agent call with the child workflow future
// started for it, so the caller can start every child before awaiting any.
type pendingSpawn struct {
	call   LLMToolCall
	future workflow.ChildWorkflowFuture
}

// runSpawnAgents dispatches every spawn_agent call in a round as an
// ExternalAgentWorkflow child. All children are started first (the futures
// collected into a slice) and only then awaited in a second pass: the
// deterministic pattern for running child workflows in parallel. Returns the
// tool result messages for every spawn call (including rejected and failed
// ones) and the summed cost of the children that completed successfully.
func runSpawnAgents(ctx workflow.Context, wfInfo *workflow.Info, in CaseInput, sup agentcfg.Supervisor, sessionID uuid.UUID, round int, spawns []LLMToolCall) ([]ActivityMessage, float64) { //nolint:gocritic // hugeParam: mirrors the workflow-input value semantics of the config already held by the caller
	var msgs []ActivityMessage
	pending := make([]pendingSpawn, 0, len(spawns))

	for spawnIndex, call := range spawns {
		kind, _ := safeString(call.Args, argSpawnKind)
		subCfg, ok := sup.SubAgents[kind]
		if !ok {
			reason := fmt.Sprintf("unknown sub-agent kind %q", kind)
			msgs = append(msgs, buildToolMessages(call, &ExecToolResult{
				CallID: call.CallID, Name: call.Name,
				Result: "error: " + reason, Error: reason,
			})...)
			continue
		}
		childCtx := workflow.WithChildOptions(ctx, workflow.ChildWorkflowOptions{
			WorkflowID: fmt.Sprintf("%s-agent-%s-r%d-c%d", wfInfo.WorkflowExecution.ID, kind, round, spawnIndex),
			TaskQueue:  wfInfo.TaskQueueName,
			// ParentClosePolicy left at its default: children are awaited
			// below, so they always terminate along with the case.
		})
		var future workflow.ChildWorkflowFuture
		if agentcfg.BackendOf(&subCfg) == agentcfg.BackendNative {
			future = workflow.ExecuteChildWorkflow(childCtx, NativeSubAgentWorkflow, NativeSubAgentInput{
				SessionID:       uuid.Nil,
				ParentSessionID: sessionID,
				Config:          subCfg,
				PromptDir:       in.PromptDir,
				Input:           call.Args[argSpawnInput],
			})
		} else {
			future = workflow.ExecuteChildWorkflow(childCtx, ExternalAgentWorkflow, ExternalAgentInput{
				SessionID:       uuid.Nil,
				ParentSessionID: sessionID,
				Config:          subCfg,
				PromptDir:       in.PromptDir,
				Item:            call.Args[argSpawnInput],
				// Identify the sub-agent so ExecAgentCLI can rehydrate its
				// stripped MCP secrets from the worker's unstripped parent
				// config (subCfg.Name is empty). in.Task is the parent task's
				// config name; kind is its key under Supervisor.SubAgents.
				SubAgentParent: in.Task,
				SubAgentKind:   kind,
			})
		}
		pending = append(pending, pendingSpawn{call: call, future: future})
	}

	var totalCost float64
	for _, p := range pending {
		var oc Outcome
		if err := p.future.Get(ctx, &oc); err != nil {
			msgs = append(msgs, buildToolMessages(p.call, &ExecToolResult{
				CallID: p.call.CallID, Name: p.call.Name,
				Result: "error: " + err.Error(), Error: err.Error(),
			})...)
			continue
		}
		totalCost += oc.TotalCostUSD
		msgs = append(msgs, buildToolMessages(p.call, &ExecToolResult{
			CallID: p.call.CallID, Name: p.call.Name,
			Result: normalizeSpawnOutcome(oc),
		})...)
	}
	return msgs, totalCost
}

// normalizeSpawnOutcome reduces a child's Outcome to the small, deterministic
// JSON payload the supervisor LLM sees as the spawn_agent tool result.
// cancel_reason and failed_at are included only when non-empty, so a failed
// or cancelled child tells the supervisor WHY and it can route accordingly
// (retry, alternative kind, escalate) instead of guessing.
func normalizeSpawnOutcome(oc Outcome) string { //nolint:gocritic // hugeParam: Outcome is already value-typed on the call path (child workflow result)
	payload := map[string]any{
		"status":   oc.Status,
		"cost_usd": oc.TotalCostUSD,
		"output":   oc.PhaseOutputs,
	}
	if oc.CancelReason != "" {
		payload["cancel_reason"] = oc.CancelReason
	}
	if oc.FailedAt != "" {
		payload["failed_at"] = oc.FailedAt
	}
	return formatToolResult(payload)
}

// runDeclaredTools executes each declared command call sequentially via
// RunDeclaredCommand, one activity call per tool, with a per-tool retry
// policy: a non-idempotent tool gets a single attempt, an idempotent one may
// retry. An activity error (the command failed to start or was cancelled) is
// reported to the LLM as an error tool result; a rejected or non-zero-exit
// call comes back as an ordinary result and is formatted the same way.
func runDeclaredTools(ctx workflow.Context, declaredByName map[string]agentcfg.DeclaredTool, declared []LLMToolCall) []ActivityMessage {
	var msgs []ActivityMessage
	for _, call := range declared {
		tool := declaredByName[call.Name]
		maxAttempts := int32(declaredToolSingleAttempt)
		if tool.Idempotent {
			maxAttempts = declaredToolIdempotentAttempts
		}
		toolCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
			StartToCloseTimeout: declaredToolTimeout,
			RetryPolicy: &temporal.RetryPolicy{
				InitialInterval:    time.Second,
				BackoffCoefficient: defaultRetryBackoff,
				MaximumAttempts:    maxAttempts,
			},
		})

		var result RunDeclaredCommandResult
		if err := workflow.ExecuteActivity(toolCtx, "RunDeclaredCommand", RunDeclaredCommandArgs{
			Tool: tool, Args: call.Args,
		}).Get(ctx, &result); err != nil {
			msgs = append(msgs, buildToolMessages(call, &ExecToolResult{
				CallID: call.CallID, Name: call.Name,
				Result: "error: " + err.Error(), Error: err.Error(),
			})...)
			continue
		}
		msgs = append(msgs, buildToolMessages(call, &ExecToolResult{
			CallID: call.CallID, Name: call.Name,
			Result: formatDeclaredCommandResult(result),
		})...)
	}
	return msgs
}

// formatDeclaredCommandResult renders a RunDeclaredCommandResult as the JSON
// tool result string the supervisor LLM sees, in every non-activity-error
// case (rejected calls and non-zero exits included).
func formatDeclaredCommandResult(result RunDeclaredCommandResult) string {
	payload := map[string]any{
		"exit_code": result.ExitCode,
		"stdout":    result.Stdout,
		"stderr":    result.Stderr,
	}
	return formatToolResult(payload)
}

// runUnknownToolCalls reports every call whose name is neither a reserved
// built-in nor a declared tool as an error result, so the LLM sees why the
// call did not run and can retry with a valid tool name. terminalNames are
// the calling loop's terminal tools: a terminal-name call lands here only
// when classifyToolCalls already honored another terminal call this round,
// so it gets an accurate message instead of the generic unknown-tool one.
func runUnknownToolCalls(unknown []LLMToolCall, terminalNames []string) []ActivityMessage {
	msgs := make([]ActivityMessage, 0, len(unknown)*messagesPerToolCall)
	for _, call := range unknown {
		reason := fmt.Sprintf("unknown tool %q", call.Name)
		if slices.Contains(terminalNames, call.Name) {
			reason = fmt.Sprintf("%s was not run: a terminal tool was already honored this round; only the first terminal call runs", call.Name)
		}
		msgs = append(msgs, buildToolMessages(call, &ExecToolResult{
			CallID: call.CallID, Name: call.Name,
			Result: "error: " + reason, Error: reason,
		})...)
	}
	return msgs
}

// isValidRecordOutcomeStatus reports whether status is one of the terminal
// statuses record_outcome accepts.
func isValidRecordOutcomeStatus(status string) bool {
	return slices.Contains(recordOutcomeStatuses, status)
}

// persistInvalidRecordOutcome persists an error tool result for a
// record_outcome call whose status the LLM got wrong, under its own
// idempotency key so it does not collide with the round's normal persist.
// The case does not finalize: the loop continues so the LLM can retry with a
// valid status.
func persistInvalidRecordOutcome(ctx, actCtx workflow.Context, sessionID uuid.UUID, round int, idemPrefix string, terminal LLMToolCall, status string) error { //nolint:gocritic // hugeParam: LLMToolCall is value-semantic on this call path
	reason := fmt.Sprintf("invalid record_outcome status %q; must be one of %v", status, recordOutcomeStatuses)
	return persistRejectedTerminal(ctx, actCtx, sessionID, round, idemPrefix, "invalid-status", terminal, reason)
}

// recordOutcomePayload marshals the LLM-provided "outcome" argument, if any,
// to the json.RawMessage FinalizeCase stores. A call with no outcome
// argument records a JSON null.
func recordOutcomePayload(args map[string]any) (json.RawMessage, error) {
	raw, ok := args[argRecordOutcome]
	if !ok {
		return json.RawMessage("null"), nil
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("marshal record_outcome outcome: %w", err)
	}
	return b, nil
}

// finalizeCase writes the case's terminal outcome to the task_state ledger
// via the existing FinalizeCase activity and returns the workflow result.
func finalizeCase(ctx workflow.Context, in CaseInput, status string, outcome json.RawMessage) (CaseOutcome, error) { //nolint:gocritic // hugeParam: workflow inputs are value-semantic for Temporal serialization
	finalizeOpts := shortActivityCtx(ctx)
	if err := workflow.ExecuteActivity(finalizeOpts, "FinalizeCase", FinalizeCaseArgs{
		Task:         in.Task,
		CandidateKey: in.CandidateKey,
		Status:       status,
		Outcome:      outcome,
	}).Get(ctx, nil); err != nil {
		return CaseOutcome{}, err
	}
	return CaseOutcome{Status: status}, nil
}

// finalizeNeedsAttention finalizes the case as needs_attention with reason
// recorded in the outcome, for any of the three guard trips (max_duration,
// cost_cap, max_rounds).
func finalizeNeedsAttention(ctx workflow.Context, in CaseInput, reason string) (CaseOutcome, error) { //nolint:gocritic // hugeParam: workflow inputs are value-semantic for Temporal serialization
	b, err := json.Marshal(map[string]string{"reason": reason})
	if err != nil {
		return CaseOutcome{}, err
	}
	return finalizeCase(ctx, in, store.TaskStatusNeedsAttention, b)
}
