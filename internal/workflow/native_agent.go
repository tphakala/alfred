package workflow

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/tphakala/alfred/internal/agentcfg"
	"github.com/tphakala/alfred/internal/ctxbuild"
	"go.temporal.io/sdk/workflow"
)

// NativeSubAgentInput is the entry shape for NativeSubAgentWorkflow. It
// mirrors the fields ExternalAgentInput carries for a cli sub-agent (session
// identity, parent link, the sub-agent config, the prompt dir, and the spawn
// input) so runSpawnAgents can dispatch either backend from the same call
// site.
type NativeSubAgentInput struct {
	SessionID       uuid.UUID
	ParentSessionID uuid.UUID
	Config          agentcfg.AgentConfig
	PromptDir       string
	Input           any
}

// nativeTerminalNames are a native sub-agent's two built-in terminal tools.
var nativeTerminalNames = []string{agentcfg.ReservedToolSubmitResult, agentcfg.ReservedToolReportFailure}

// defaultNativeSystemInstruction is the defense-in-depth fallback when
// Prompt.Base is empty. Validated configs always set native.prompt.base
// (validateNativeAgent requires it), so this only fires for configs that
// bypassed validation.
const defaultNativeSystemInstruction = "You are a sub-agent. Complete the task described in the input. " +
	"Call submit_result exactly once with your final output matching the declared schema, or " +
	"report_failure with a concrete reason if the task cannot be completed."

// NativeSubAgentWorkflow is a backend: native sub-agent: an autonomous LLM
// ReAct loop on Alfred's native providers, spawned as a child of a Case
// Supervisor. It reuses the shared runAgentLoop; its terminal tools are
// submit_result (bound to the declared output schema) and report_failure (the
// explicit failure hatch) instead of record_outcome. It returns the same
// Outcome envelope a cli sub-agent (ExternalAgentWorkflow) returns, so the
// supervisor treats both backends identically. Its own LLM token spend is
// metered in USD by CaseLLMStream (a cost_cap trip maps to budget_exceeded
// below) and surfaced up via Outcome.TotalCostUSD, so the parent supervisor's
// cost cap counts a native child's spend the same way it counts a cli child's.
func NativeSubAgentWorkflow(ctx workflow.Context, in NativeSubAgentInput) (Outcome, error) { //nolint:gocognit,gocyclo,gocritic // gocognit/gocyclo: inherently-complex workflow orchestrator; hugeParam: workflow inputs are value-semantic for Temporal serialization
	na := in.Config.Native
	maxRounds := na.MaxRounds
	if maxRounds <= 0 {
		// Shared default with the supervisor loop (both are runAgentLoop
		// parameterizations).
		maxRounds = defaultAgentLoopMaxRounds
	}
	declaredByName := make(map[string]agentcfg.DeclaredTool, len(na.Tools))
	for _, t := range na.Tools {
		declaredByName[t.Name] = t
	}

	sessionID := in.SessionID
	if sessionID == uuid.Nil {
		encoded := workflow.SideEffect(ctx, func(_ workflow.Context) any {
			return uuid.New()
		})
		if err := encoded.Get(&sessionID); err != nil {
			return Outcome{}, err
		}
	}

	wfInfo := workflow.GetInfo(ctx)
	shortActCtx := shortActivityCtx(ctx)

	// Create the session row (FK target for messages), mirroring the cli
	// sub-agent path. The spawn input is recorded as the fanout item so the
	// child session row carries its input, same as a cli fanout child.
	var inputRaw json.RawMessage
	if in.Input != nil {
		b, err := json.Marshal(in.Input)
		if err != nil {
			return Outcome{}, fmt.Errorf("marshal native sub-agent input: %w", err)
		}
		inputRaw = b
	}
	if err := workflow.ExecuteActivity(shortActCtx, "CreateAgentSession", CreateAgentSessionArgs{
		SessionID:       sessionID,
		WorkflowID:      wfInfo.WorkflowExecution.ID,
		RunID:           wfInfo.WorkflowExecution.RunID,
		ParentSessionID: in.ParentSessionID,
		FanoutItem:      inputRaw,
	}).Get(ctx, nil); err != nil {
		return Outcome{Status: OutcomeStatusSessionCreateFailed}, err
	}

	actCtx := defaultLoopActivityCtx(ctx)

	systemInstruction := defaultNativeSystemInstruction
	if na.Prompt.Base != "" {
		// The spec's template mechanism for native input: the prompt template
		// may reference {{ .input }} (parity with cli's {{ .item }}). The
		// input is ALSO always the loop's first user message below, so a
		// prompt that ignores the variable still sees the input.
		vars := map[string]any{"input": in.Input}
		if err := workflow.ExecuteActivity(actCtx, "RenderCasePrompt",
			in.PromptDir, na.Prompt, vars).Get(ctx, &systemInstruction); err != nil {
			return Outcome{}, err
		}
	}

	initialContent := string(inputRaw)
	if initialContent == "" {
		initialContent = "Begin working the task."
	}
	idemPrefix := fmt.Sprintf("agent-%s", sessionID)
	if err := workflow.ExecuteActivity(actCtx, "PersistRound", PersistRoundRequest{
		SessionID:      sessionID,
		IdempotencyKey: idemPrefix + "-init",
		Messages: []ActivityMessage{{
			Role:          ctxbuild.RoleUser,
			Content:       initialContent,
			TokenEstimate: estimateTokens(initialContent),
		}},
	}).Get(ctx, nil); err != nil {
		return Outcome{}, err
	}

	handleNativeTools := func(loopCtx workflow.Context, round int, text string, calls []LLMToolCall) (float64, *loopStop, error) {
		terminal, _, declared, unknown := classifyToolCalls(calls, nativeTerminalNames, "", declaredByName)

		var toolMessages []ActivityMessage //nolint:prealloc // size spans several append sources; not worth a speculative cap
		toolMessages = append(toolMessages, runDeclaredTools(loopCtx, declaredByName, declared)...)
		toolMessages = append(toolMessages, runUnknownToolCalls(unknown, nativeTerminalNames)...)

		if err := persistLoopRound(loopCtx, actCtx, sessionID, round, idemPrefix, loopRoundMessages(text, len(calls) > 0, toolMessages)); err != nil {
			return 0, nil, err
		}
		if terminal == nil {
			return 0, nil, nil
		}
		if terminal.Name == agentcfg.ReservedToolSubmitResult {
			problems := missingRequiredFields(na.Output.Schema, terminal.Args)
			problems = append(problems, submitResultTypeMismatches(na.Output.Schema, terminal.Args)...)
			if len(problems) > 0 {
				if err := persistSubmitResultRejection(loopCtx, actCtx, sessionID, round, idemPrefix, *terminal, problems); err != nil {
					return 0, nil, err
				}
				return 0, nil, nil
			}
		}
		// report_failure is accepted as-is; a missing reason is defaulted at
		// the Outcome mapping below rather than burning a retry round.
		acceptedResult := "result accepted"
		if terminal.Name == agentcfg.ReservedToolReportFailure {
			acceptedResult = "failure reported"
		}
		if err := persistAcceptedTerminal(loopCtx, actCtx, sessionID, round, idemPrefix, *terminal, acceptedResult); err != nil {
			return 0, nil, err
		}
		return 0, &loopStop{Terminal: terminal}, nil
	}

	outcome, err := runAgentLoop(ctx, agentLoopSpec{
		SessionID:         sessionID,
		Model:             na.Model,
		SystemInstruction: systemInstruction,
		ToolDecls:         nativeSubAgentToolDeclarations(na),
		MaxRounds:         maxRounds,
		MaxDuration:       na.MaxDuration,
		CostCapUSD:        na.CostCapUSD,
		HandleTools:       handleNativeTools,
	})
	if err != nil {
		return Outcome{}, err
	}

	if outcome.Terminal != nil {
		if outcome.Terminal.Name == agentcfg.ReservedToolReportFailure {
			reason, _ := safeString(outcome.Terminal.Args, argFailReason)
			if reason == "" {
				reason = "sub-agent reported failure without a reason"
			}
			return Outcome{Status: OutcomeStatusFailed, CancelReason: reason, TotalCostUSD: outcome.CostUSD}, nil
		}
		return Outcome{
			Status:       OutcomeStatusOK,
			PhaseOutputs: map[string]any{"result": outcome.Terminal.Args},
			TotalCostUSD: outcome.CostUSD,
		}, nil
	}

	// Guard trips never leak ledger statuses (needs_attention etc.) into the
	// child envelope. cost_cap maps to budget_exceeded, the same status the
	// cli backend uses for its budget guard; the bounded-loop guards report a
	// generic failed with the guard name as the reason (the cli backend has
	// no equivalent guard, its per-phase failures are more granular).
	if outcome.GuardTrip == guardCostCap {
		return Outcome{Status: OutcomeStatusBudgetExceeded, TotalCostUSD: outcome.CostUSD}, nil
	}
	return Outcome{Status: OutcomeStatusFailed, CancelReason: outcome.GuardTrip, TotalCostUSD: outcome.CostUSD}, nil
}

// persistSubmitResultRejection persists an error tool result for a
// submit_result call that failed acceptance (missing required fields or
// top-level type mismatches), under its own idempotency key so it does not
// collide with the round's normal persist. The loop continues so the LLM can
// resubmit corrected output, bounded by max_rounds.
func persistSubmitResultRejection(ctx, actCtx workflow.Context, sessionID uuid.UUID, round int, idemPrefix string, terminal LLMToolCall, problems []string) error { //nolint:gocritic // hugeParam: LLMToolCall is value-semantic on this call path
	reason := fmt.Sprintf("submit_result rejected: %s; resubmit with the declared output schema satisfied", strings.Join(problems, "; "))
	return persistRejectedTerminal(ctx, actCtx, sessionID, round, idemPrefix, "rejected", terminal, reason)
}
