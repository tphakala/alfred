package workflow

import (
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/tphakala/alfred/internal/agentcfg"
	"github.com/tphakala/alfred/internal/ctxbuild"
	"github.com/tphakala/alfred/internal/llm"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

// Guard-trip reasons returned by runAgentLoop when no terminal tool was
// accepted. The supervisor records these in its needs_attention ledger
// outcome; the native loop maps them to a failed Outcome reason (cost_cap
// maps to budget_exceeded). guardNoToolCalls was added in #26.
const (
	guardMaxRounds   = "max_rounds"
	guardMaxDuration = "max_duration"
	guardCostCap     = "cost_cap"
	// guardNoToolCalls trips when the model returns maxNoToolCallNudges+1
	// consecutive rounds with no tool calls: the loop nudges the model back
	// toward its terminal tool a bounded number of times, then gives up rather
	// than re-invoke the LLM on a model-side-last context (#26).
	guardNoToolCalls = "no_tool_calls"
)

// maxNoToolCallNudges bounds how many consecutive no-tool-call rounds the loop
// nudges (persists a synthetic user turn to force progress) before tripping
// guardNoToolCalls. The trip happens on the (maxNoToolCallNudges+1)-th
// consecutive no-tool-call round. Any round with a tool call resets the count.
const maxNoToolCallNudges = 2

// loopStop is HandleTools' signal that the loop should end after this round.
// Terminal is the accepted terminal tool call (record_outcome for the
// supervisor, submit_result or report_failure for a native sub-agent).
type loopStop struct {
	Terminal *LLMToolCall
}

// agentLoopSpec parameterizes one ReAct agent loop. Everything that differs
// between the Case Supervisor (CaseWorkflow) and a native sub-agent
// (NativeSubAgentWorkflow) is either a scalar here or captured by HandleTools.
type agentLoopSpec struct {
	SessionID         uuid.UUID
	Model             string
	SystemInstruction string
	ToolDecls         []*llm.FunctionDeclaration
	MaxRounds         int
	MaxDuration       time.Duration
	CostCapUSD        float64

	// NudgeMessage is the user-side text runAgentLoop persists on a
	// no-tool-call round to steer the model back to its terminal tool. It must
	// name that terminal tool (record_outcome for the supervisor;
	// submit_result/report_failure for a native sub-agent). Required: an empty
	// value would persist an empty user turn (the store only short-circuits an
	// empty message LIST, not an empty message).
	NudgeMessage string
	// IdempotencyPrefix is the loop's persist-key prefix ("case-<sid>" /
	// "agent-<sid>"). It MUST match the prefix the caller's HandleTools closure
	// persists under, so the nudge key ("<prefix>-<round>-nudge") shares the
	// round's key namespace.
	IdempotencyPrefix string

	// HandleTools processes one round: it classifies the LLM's tool calls,
	// runs the non-terminal ones, persists the round (it owns round
	// persistence, including the no-tool-call round's model message and the
	// accepted- or rejected-terminal call/result; the one exception is the
	// synthetic no-tool-call nudge, which runAgentLoop persists itself via
	// persistNudge), and decides whether the round is terminal. It returns the
	// cost incurred this round, a non-nil *loopStop
	// when the loop should end after this round, and any activity error. A
	// terminal call the handler rejects (invalid record_outcome status,
	// submit_result schema gap) is reported to the LLM as a normal error tool
	// result with stop left nil, so the loop continues, bounded by MaxRounds.
	HandleTools func(ctx workflow.Context, round int, text string, calls []LLMToolCall) (cost float64, stop *loopStop, err error)
}

// agentLoopOutcome reports why runAgentLoop returned without error. Exactly
// one of Terminal (an accepted terminal tool call) or GuardTrip (a guard
// reason constant) is set. CostUSD is the loop's total metered spend (own LLM
// token cost plus any cost HandleTools reported), so a native sub-agent can
// surface it up to the parent supervisor's cost cap the way a cli child does.
type agentLoopOutcome struct {
	Terminal  *LLMToolCall
	GuardTrip string
	CostUSD   float64
}

// runAgentLoop runs the shared ReAct round loop: between-round guards
// (max_duration, cost_cap, max_rounds), BuildContext, CaseLLMStream, then
// HandleTools. It also enforces an in-round no-progress guard: after
// maxNoToolCallNudges consecutive no-tool-call rounds it trips guardNoToolCalls
// rather than re-invoke the LLM on a model-side-last context (#26). The caller
// owns setup (session claim/create, prompt render, initial user message) and
// finalization (ledger write for the supervisor, Outcome envelope for a native
// sub-agent).
//
// The MaxDuration window starts when the loop starts, so the caller's setup
// activities (claim, render, initial persist) are excluded. Pre-extraction
// CaseWorkflow measured from the top of the workflow function; the drift is a
// few activity round-trips and is deterministic either way (workflow.Now).
func runAgentLoop(ctx workflow.Context, spec agentLoopSpec) (agentLoopOutcome, error) { //nolint:gocritic // hugeParam: spec is value-semantic like the workflow inputs it is derived from
	startTime := workflow.Now(ctx)

	actCtx := defaultLoopActivityCtx(ctx)
	llmActCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: llmActivityTimeout,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    time.Second,
			BackoffCoefficient: defaultRetryBackoff,
			MaximumAttempts:    defaultRetryMaxAttempts,
		},
	})

	var cumulativeCost float64
	// noToolRounds counts CONSECUTIVE no-tool-call rounds; it drives the
	// no-progress nudge/guard below and resets on any round with a tool call.
	var noToolRounds int
	// Establishes a version-marker baseline at this sequence point (#65); no
	// branch, the cost-cap guard and cumulativeCost accumulation below
	// (added by #66) are unconditional -- see chat.go's chat-tool-idempotency
	// marker comment for why. Covers both CaseWorkflow and
	// NativeSubAgentWorkflow, the two callers of this shared loop.
	workflow.GetVersion(ctx, "loop-cost-cap-guard", workflow.DefaultVersion, 1)
	for round := 1; round <= spec.MaxRounds; round++ {
		if spec.MaxDuration > 0 && workflow.Now(ctx).Sub(startTime) >= spec.MaxDuration {
			return agentLoopOutcome{GuardTrip: guardMaxDuration, CostUSD: cumulativeCost}, nil
		}
		// Enforcement is post-hoc, like ExternalAgentWorkflow's budget check:
		// a round already in flight may overspend before this guard trips on
		// the next round, since the cap is only checked between rounds.
		// cumulativeCost sums the loop's own LLM token spend (priced in USD by
		// CaseLLMStream, see CaseActivities.priceRound) plus whatever spawned
		// sub-agent cost HandleTools reports. A model with no pricing entry
		// contributes zero (CaseLLMStream logs a warning once), so for such a
		// model the cap is inert and the loop is bounded by MaxRounds/MaxDuration.
		if spec.CostCapUSD > 0 && cumulativeCost >= spec.CostCapUSD {
			return agentLoopOutcome{GuardTrip: guardCostCap, CostUSD: cumulativeCost}, nil
		}

		var ctxResult BuildContextResult
		if err := workflow.ExecuteActivity(actCtx, "BuildContext",
			spec.SessionID, DefaultTokenBudget, "").Get(ctx, &ctxResult); err != nil {
			return agentLoopOutcome{}, err
		}

		var llmResult CaseLLMStreamResult
		if err := workflow.ExecuteActivity(llmActCtx, "CaseLLMStream", CaseLLMStreamRequest{
			SessionID:         spec.SessionID,
			TurnID:            agentLoopTurnID,
			Round:             round,
			Messages:          ctxResult.Messages,
			Tools:             spec.ToolDecls,
			ToolConfig:        nil, // never force text-only: the loop may call its terminal tool on any round
			SystemInstruction: spec.SystemInstruction,
			Model:             spec.Model,
		}).Get(ctx, &llmResult); err != nil {
			return agentLoopOutcome{}, err
		}
		cumulativeCost += llmResult.CostUSD

		cost, stop, err := spec.HandleTools(ctx, round, llmResult.Text, llmResult.ToolCalls)
		if err != nil {
			return agentLoopOutcome{}, err
		}
		cumulativeCost += cost
		if stop != nil {
			return agentLoopOutcome{Terminal: stop.Terminal, CostUSD: cumulativeCost}, nil
		}

		gstop, gout, gerr := trackNoToolCallRound(ctx, actCtx, spec, round, len(llmResult.ToolCalls) > 0, &noToolRounds, cumulativeCost)
		if gerr != nil {
			return agentLoopOutcome{}, gerr
		}
		if gstop {
			return gout, nil
		}
	}
	return agentLoopOutcome{GuardTrip: guardMaxRounds, CostUSD: cumulativeCost}, nil
}

// trackNoToolCallRound advances the consecutive no-tool-call bookkeeping after
// a round (#26). When the round had tool calls it resets the counter and lets
// the loop continue. When it had none, the round persisted only a model-side
// turn, so the next BuildContext would end model-side and CaseLLMStream would
// reject it (llm.ErrLastRoleNotUser); this nudges the model back toward its
// terminal tool with a synthetic user turn, bounded, and after
// maxNoToolCallNudges consecutive such rounds returns stop=true with the
// guardNoToolCalls outcome instead of re-invoking on a model-side-last context.
//
// The GetVersion gate genuinely branches, unlike the unconditional baseline
// markers in runAgentLoop: an in-flight pre-fix execution that already hit this
// path has no marker in its history, so it resolves to DefaultVersion, skips
// the nudge/trip, and replays its original crashing sequence deterministically.
// The DefaultVersion arm is not covered by a test (TestWorkflowEnvironment
// always resolves a fresh execution to version 1, and no checked-in replay
// fixture contains a no-tool-call round); its only job is to keep those doomed
// in-flight histories from a non-determinism panic on upgrade.
func trackNoToolCallRound(ctx, actCtx workflow.Context, spec agentLoopSpec, round int, hadToolCalls bool, noToolRounds *int, cumulativeCost float64) (stop bool, out agentLoopOutcome, err error) { //nolint:gocritic // hugeParam: spec is value-semantic like runAgentLoop's own signature
	if hadToolCalls {
		*noToolRounds = 0
		return false, agentLoopOutcome{}, nil
	}
	if workflow.GetVersion(ctx, "loop-no-tool-call-nudge", workflow.DefaultVersion, 1) != 1 {
		return false, agentLoopOutcome{}, nil // old history: skip, replay the original path
	}
	*noToolRounds++
	if *noToolRounds > maxNoToolCallNudges {
		return true, agentLoopOutcome{GuardTrip: guardNoToolCalls, CostUSD: cumulativeCost}, nil
	}
	if perr := persistNudge(ctx, actCtx, spec.SessionID, round, spec.IdempotencyPrefix, spec.NudgeMessage); perr != nil {
		return false, agentLoopOutcome{}, perr
	}
	return false, agentLoopOutcome{}, nil
}

// defaultLoopActivityCtx returns the standard activity options shared by the
// agent loop and its callers' out-of-loop persistence calls.
func defaultLoopActivityCtx(ctx workflow.Context) workflow.Context {
	return workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: defaultActivityTimeout,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    time.Second,
			BackoffCoefficient: defaultRetryBackoff,
			MaximumAttempts:    defaultRetryMaxAttempts,
		},
	})
}

// shortActivityCtx returns the short-timeout, few-attempts options used for
// quick bookkeeping activities (claim, finalize, session create).
func shortActivityCtx(ctx workflow.Context) workflow.Context {
	return workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: loopActivityTimeout,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: loopActivityMaxAttempt},
	})
}

// persistLoopRound persists one round's messages under the loop's idempotency
// prefix ("case-<sid>" or "agent-<sid>"). Shared by every HandleTools
// implementation.
func persistLoopRound(ctx, actCtx workflow.Context, sessionID uuid.UUID, round int, idemPrefix string, msgs []ActivityMessage) error {
	return workflow.ExecuteActivity(actCtx, "PersistRound", PersistRoundRequest{
		SessionID:      sessionID,
		Round:          round,
		IdempotencyKey: fmt.Sprintf("%s-%d", idemPrefix, round),
		Messages:       msgs,
	}).Get(ctx, nil)
}

// persistNudge persists a single synthetic user-side turn on a no-tool-call
// round so the next BuildContext ends user-side (satisfying the llm last-role
// guard) and the model gets one concrete instruction to call a tool. It uses
// its own idempotency key ("<prefix>-<round>-nudge") so it never collides with
// the round's model-message persist ("<prefix>-<round>") or a terminal persist
// ("<prefix>-<round>-<suffix>"). msg comes from agentLoopSpec.NudgeMessage and
// must name the loop's terminal tool.
func persistNudge(ctx, actCtx workflow.Context, sessionID uuid.UUID, round int, idemPrefix, msg string) error {
	return workflow.ExecuteActivity(actCtx, "PersistRound", PersistRoundRequest{
		SessionID:      sessionID,
		Round:          round,
		IdempotencyKey: fmt.Sprintf("%s-%d-nudge", idemPrefix, round),
		Messages: []ActivityMessage{{
			Role:          ctxbuild.RoleUser,
			Content:       msg,
			TokenEstimate: estimateTokens(msg),
		}},
	}).Get(ctx, nil)
}

// persistTerminalCall persists a single terminal tool call/result pair under
// its own idempotency key (idemPrefix-round-keySuffix) so it does not collide
// with the round's normal persist ("idemPrefix-round"). Shared by the
// accepted- and rejected-terminal helpers; the caller supplies the
// ExecToolResult (a success confirmation or an error) recorded for the call.
func persistTerminalCall(ctx, actCtx workflow.Context, sessionID uuid.UUID, round int, idemPrefix, keySuffix string, terminal LLMToolCall, result *ExecToolResult) error { //nolint:gocritic // hugeParam: LLMToolCall is value-semantic on this call path
	return workflow.ExecuteActivity(actCtx, "PersistRound", PersistRoundRequest{
		SessionID:      sessionID,
		Round:          round,
		IdempotencyKey: fmt.Sprintf("%s-%d-%s", idemPrefix, round, keySuffix),
		Messages:       buildToolMessages(terminal, result),
	}).Get(ctx, nil)
}

// persistAcceptedTerminal persists the accepted terminal tool call/result pair
// (record_outcome for the supervisor, submit_result/report_failure for a
// native sub-agent) so the session transcript records the call that ended the
// loop, not just the durable task_state ledger outcome. result is a short
// success confirmation. It uses a distinct "accepted" key suffix so it does
// not collide with the round's normal persist or a rejected-terminal persist.
func persistAcceptedTerminal(ctx, actCtx workflow.Context, sessionID uuid.UUID, round int, idemPrefix string, terminal LLMToolCall, result string) error { //nolint:gocritic // hugeParam: LLMToolCall is value-semantic on this call path
	// Establishes a version-marker baseline at this sequence point (#65); no
	// branch, this persist call (added by #70) is unconditional -- see
	// chat.go's chat-tool-idempotency marker comment for why. Covers both
	// CaseWorkflow and NativeSubAgentWorkflow, the two callers of this
	// shared helper.
	workflow.GetVersion(ctx, "loop-persist-accepted-terminal", workflow.DefaultVersion, 1)
	return persistTerminalCall(ctx, actCtx, sessionID, round, idemPrefix, "accepted", terminal, &ExecToolResult{
		CallID: terminal.CallID, Name: terminal.Name, Result: result,
	})
}

// persistRejectedTerminal persists an error tool result for a terminal tool
// call the loop rejected (invalid record_outcome status, submit_result schema
// gap), under its own idempotency key (idemPrefix-round-keySuffix) so it does
// not collide with the round's normal persist. The loop continues so the LLM
// can retry, bounded by MaxRounds.
func persistRejectedTerminal(ctx, actCtx workflow.Context, sessionID uuid.UUID, round int, idemPrefix, keySuffix string, terminal LLMToolCall, reason string) error { //nolint:gocritic // hugeParam: LLMToolCall is value-semantic on this call path
	return persistTerminalCall(ctx, actCtx, sessionID, round, idemPrefix, keySuffix, terminal, &ExecToolResult{
		CallID: terminal.CallID, Name: terminal.Name,
		Result: "error: " + reason, Error: reason,
	})
}

// loopRoundMessages builds a round's persist payload. A round with NO tool
// calls persists exactly one model message even when the text is empty
// (preserving CaseWorkflow's pre-extraction behavior where an empty-text
// round still records that the round happened). A round WITH tool calls
// defers to buildRoundMessages: model text only when non-empty, then the
// tool call/result pairs; a terminal-only round with empty text therefore
// persists nothing, exactly as before the extraction (the store's
// AppendMessagesIdempotent short-circuits on an empty list).
func loopRoundMessages(text string, hadToolCalls bool, toolMsgs []ActivityMessage) []ActivityMessage {
	if !hadToolCalls {
		return []ActivityMessage{{
			Role:          ctxbuild.RoleModel,
			Content:       text,
			TokenEstimate: estimateTokens(text),
		}}
	}
	return buildRoundMessages(text, toolMsgs)
}

// classifyToolCalls partitions one round's tool calls, preserving slice order
// within each bucket so execution order is driven by the ordered slices rather
// than by ranging a map. terminalNames are the loop's terminal tools (the
// supervisor has one, a native sub-agent has two); the FIRST call to any of
// them becomes terminal and every later terminal-name call lands in unknown.
// spawnName is the spawn tool ("" for a loop with no spawn tool, e.g. a
// native sub-agent, so a spawn_agent call there routes to unknown).
// declaredByName holds the deployment-declared command tools.
func classifyToolCalls(calls []LLMToolCall, terminalNames []string, spawnName string, declaredByName map[string]agentcfg.DeclaredTool) (terminal *LLMToolCall, spawns, declared, unknown []LLMToolCall) {
	for i := range calls {
		call := calls[i]
		switch {
		case slices.Contains(terminalNames, call.Name):
			if terminal == nil {
				terminal = &calls[i]
			} else {
				unknown = append(unknown, call)
			}
		case spawnName != "" && call.Name == spawnName:
			spawns = append(spawns, call)
		default:
			if _, ok := declaredByName[call.Name]; ok {
				declared = append(declared, call)
			} else {
				unknown = append(unknown, call)
			}
		}
	}
	return terminal, spawns, declared, unknown
}
