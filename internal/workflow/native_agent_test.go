package workflow

import (
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"github.com/tphakala/alfred/internal/agentcfg"
	"github.com/tphakala/alfred/internal/ctxbuild"
	"go.temporal.io/sdk/testsuite"
)

// registerNativeAgentActivities registers everything NativeSubAgentWorkflow
// calls: the case-loop activities plus CreateAgentSession (owned by
// ExternalAgentActivities, shared with the cli backend).
func registerNativeAgentActivities(env *testsuite.TestWorkflowEnvironment) (caseTestActs, *ExternalAgentActivities) {
	acts := registerCaseActivities(env)
	extActs := &ExternalAgentActivities{}
	env.RegisterActivity(extActs.CreateAgentSession)
	return acts, extActs
}

func nativeTestConfig() agentcfg.AgentConfig {
	return agentcfg.AgentConfig{
		Backend: agentcfg.BackendNative,
		Native: agentcfg.NativeAgent{
			LoopConfig: agentcfg.LoopConfig{
				Model: "test-model",
				// Prompt.Base deliberately empty: this bypasses Validate (which
				// requires it) to exercise the workflow's defense-in-depth
				// default instruction without mocking RenderCasePrompt.
				MaxRounds: 3,
			},
			Output: agentcfg.Output{Schema: map[string]any{
				"type":     "object",
				"required": []any{"summary"},
				"properties": map[string]any{
					"summary": map[string]any{"type": "string"},
				},
			}},
		},
	}
}

func nativeSubmitCall(callID string, args map[string]any) LLMToolCall {
	return LLMToolCall{CallID: callID, Name: agentcfg.ReservedToolSubmitResult, Args: args}
}

func TestNativeSubAgentWorkflow_SubmitResultTerminates(t *testing.T) {
	ts := &testsuite.WorkflowTestSuite{}
	env := ts.NewTestWorkflowEnvironment()
	acts, extActs := registerNativeAgentActivities(env)

	env.OnActivity(extActs.CreateAgentSession, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(acts.chatActs.PersistRound, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(acts.chatActs.BuildContext, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(&BuildContextResult{}, nil)
	env.OnActivity(acts.caseActs.CaseLLMStream, mock.Anything, mock.Anything).Return(
		&CaseLLMStreamResult{Text: "done", ToolCalls: []LLMToolCall{
			nativeSubmitCall("t1-c0", map[string]any{"summary": "all good"}),
		}}, nil,
	).Once()

	env.ExecuteWorkflow(NativeSubAgentWorkflow, NativeSubAgentInput{
		ParentSessionID: uuid.New(),
		Config:          nativeTestConfig(),
		Input:           map[string]any{"key": "123"},
	})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	var out Outcome
	require.NoError(t, env.GetWorkflowResult(&out))
	assert.Equal(t, OutcomeStatusOK, out.Status)
	result, ok := out.PhaseOutputs["result"].(map[string]any)
	require.True(t, ok, "result should be a map, got %T", out.PhaseOutputs["result"])
	assert.Equal(t, "all good", result["summary"])
}

func TestNativeSubAgentWorkflow_ReportFailure(t *testing.T) {
	ts := &testsuite.WorkflowTestSuite{}
	env := ts.NewTestWorkflowEnvironment()
	acts, extActs := registerNativeAgentActivities(env)

	env.OnActivity(extActs.CreateAgentSession, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(acts.chatActs.PersistRound, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(acts.chatActs.BuildContext, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(&BuildContextResult{}, nil)
	env.OnActivity(acts.caseActs.CaseLLMStream, mock.Anything, mock.Anything).Return(
		&CaseLLMStreamResult{ToolCalls: []LLMToolCall{{
			CallID: "t1-c0", Name: agentcfg.ReservedToolReportFailure,
			Args: map[string]any{argFailReason: "target does not exist"},
		}}}, nil,
	).Once()

	env.ExecuteWorkflow(NativeSubAgentWorkflow, NativeSubAgentInput{Config: nativeTestConfig(), Input: "go"})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	var out Outcome
	require.NoError(t, env.GetWorkflowResult(&out))
	assert.Equal(t, OutcomeStatusFailed, out.Status)
	assert.Equal(t, "target does not exist", out.CancelReason)
}

func TestNativeSubAgentWorkflow_SubmitResultGapRetries(t *testing.T) {
	ts := &testsuite.WorkflowTestSuite{}
	env := ts.NewTestWorkflowEnvironment()
	acts, extActs := registerNativeAgentActivities(env)

	env.OnActivity(extActs.CreateAgentSession, mock.Anything, mock.Anything).Return(nil)

	getPersists := capturePersistRounds(t, env, acts.chatActs.PersistRound)

	env.OnActivity(acts.chatActs.BuildContext, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(&BuildContextResult{}, nil)
	// Round 1: missing required "summary" -> rejected, loop continues.
	env.OnActivity(acts.caseActs.CaseLLMStream, mock.Anything, mock.Anything).Return(
		&CaseLLMStreamResult{ToolCalls: []LLMToolCall{nativeSubmitCall("t1-c0", map[string]any{})}}, nil,
	).Once()
	// Round 2: complete -> accepted.
	env.OnActivity(acts.caseActs.CaseLLMStream, mock.Anything, mock.Anything).Return(
		&CaseLLMStreamResult{ToolCalls: []LLMToolCall{nativeSubmitCall("t2-c0", map[string]any{"summary": "now complete"})}}, nil,
	).Once()

	env.ExecuteWorkflow(NativeSubAgentWorkflow, NativeSubAgentInput{Config: nativeTestConfig(), Input: "go"})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	var out Outcome
	require.NoError(t, env.GetWorkflowResult(&out))
	assert.Equal(t, OutcomeStatusOK, out.Status)

	// The rejection round persisted a tool_result naming the schema gap so the
	// LLM could correct it: "submit_result rejected" plus the missing field.
	assert.True(t, persistedToolResultContains(getPersists(), "submit_result rejected", "summary"),
		"a tool_result explaining the submit_result schema gap must have been persisted")

	result, ok := out.PhaseOutputs["result"].(map[string]any)
	require.True(t, ok, "result should be a map, got %T", out.PhaseOutputs["result"])
	assert.Equal(t, "now complete", result["summary"])
	env.AssertExpectations(t)
}

func TestNativeSubAgentWorkflow_PersistsInitialInput(t *testing.T) {
	ts := &testsuite.WorkflowTestSuite{}
	env := ts.NewTestWorkflowEnvironment()
	acts, extActs := registerNativeAgentActivities(env)

	env.OnActivity(extActs.CreateAgentSession, mock.Anything, mock.Anything).Return(nil)

	getPersists := capturePersistRounds(t, env, acts.chatActs.PersistRound)

	env.OnActivity(acts.chatActs.BuildContext, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(&BuildContextResult{}, nil)
	env.OnActivity(acts.caseActs.CaseLLMStream, mock.Anything, mock.Anything).Return(
		&CaseLLMStreamResult{ToolCalls: []LLMToolCall{
			nativeSubmitCall("t1-c0", map[string]any{"summary": "all good"}),
		}}, nil,
	).Once()

	env.ExecuteWorkflow(NativeSubAgentWorkflow, NativeSubAgentInput{
		ParentSessionID: uuid.New(),
		Config:          nativeTestConfig(),
		Input:           map[string]any{"key": "123"},
	})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	// The spawn input is recorded as the loop's first user message, under the
	// "-init" idempotency key, before any round runs.
	persistCalls := getPersists()
	require.NotEmpty(t, persistCalls, "at least the initial-input persist must have run")
	first := persistCalls[0]
	assert.True(t, strings.HasSuffix(first.IdempotencyKey, "-init"),
		"first persist must be the initial-input round, key = %q", first.IdempotencyKey)
	require.NotEmpty(t, first.Messages)
	assert.Equal(t, ctxbuild.RoleUser, first.Messages[0].Role)
	assert.JSONEq(t, `{"key":"123"}`, first.Messages[0].Content)
}

func TestNativeSubAgentWorkflow_MaxRoundsFails(t *testing.T) {
	ts := &testsuite.WorkflowTestSuite{}
	env := ts.NewTestWorkflowEnvironment()
	acts, extActs := registerNativeAgentActivities(env)

	env.OnActivity(extActs.CreateAgentSession, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(acts.chatActs.PersistRound, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(acts.chatActs.BuildContext, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(&BuildContextResult{}, nil)
	// Never terminal: text-only rounds until max_rounds (3) exhausts.
	env.OnActivity(acts.caseActs.CaseLLMStream, mock.Anything, mock.Anything).Return(
		&CaseLLMStreamResult{Text: "thinking"}, nil)

	env.ExecuteWorkflow(NativeSubAgentWorkflow, NativeSubAgentInput{Config: nativeTestConfig(), Input: "go"})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	var out Outcome
	require.NoError(t, env.GetWorkflowResult(&out))
	assert.Equal(t, OutcomeStatusFailed, out.Status)
	assert.Equal(t, guardMaxRounds, out.CancelReason)
}

func TestNativeSubAgentWorkflow_DeclaredToolRuns(t *testing.T) {
	ts := &testsuite.WorkflowTestSuite{}
	env := ts.NewTestWorkflowEnvironment()
	acts, extActs := registerNativeAgentActivities(env)

	cfg := nativeTestConfig()
	cfg.Native.Tools = []agentcfg.DeclaredTool{
		{Name: "lookup", Command: []string{"echo", "--", "{q}"}, Idempotent: true},
	}

	env.OnActivity(extActs.CreateAgentSession, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(acts.chatActs.PersistRound, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(acts.chatActs.BuildContext, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(&BuildContextResult{}, nil)
	env.OnActivity(acts.caseActs.RunDeclaredCommand, mock.Anything, mock.Anything).
		Return(RunDeclaredCommandResult{ExitCode: 0, Stdout: "found"}, nil).Once()
	// Round 1: declared tool call. Round 2: submit_result.
	env.OnActivity(acts.caseActs.CaseLLMStream, mock.Anything, mock.Anything).Return(
		&CaseLLMStreamResult{ToolCalls: []LLMToolCall{{
			CallID: "t1-c0", Name: "lookup", Args: map[string]any{"q": "item"},
		}}}, nil,
	).Once()
	env.OnActivity(acts.caseActs.CaseLLMStream, mock.Anything, mock.Anything).Return(
		&CaseLLMStreamResult{ToolCalls: []LLMToolCall{nativeSubmitCall("t2-c0", map[string]any{"summary": "found it"})}}, nil,
	).Once()

	env.ExecuteWorkflow(NativeSubAgentWorkflow, NativeSubAgentInput{Config: cfg, Input: "go"})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	var out Outcome
	require.NoError(t, env.GetWorkflowResult(&out))
	assert.Equal(t, OutcomeStatusOK, out.Status)
	env.AssertExpectations(t)
}

// TestNativeSubAgentWorkflow_CostCapExceeded proves the loop's own metered LLM
// spend (not just spawned-child cost, which a native sub-agent never has) trips
// cost_cap and maps to budget_exceeded. Before #66 priced the loop's tokens,
// HandleTools always reported 0 cost, so this guard branch was unreachable.
func TestNativeSubAgentWorkflow_CostCapExceeded(t *testing.T) {
	ts := &testsuite.WorkflowTestSuite{}
	env := ts.NewTestWorkflowEnvironment()
	acts, extActs := registerNativeAgentActivities(env)

	cfg := nativeTestConfig()
	cfg.Native.CostCapUSD = 1.0

	env.OnActivity(extActs.CreateAgentSession, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(acts.chatActs.PersistRound, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(acts.chatActs.BuildContext, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(&BuildContextResult{}, nil)

	// Round 1: non-terminal, but reports $2.00 spend, exceeding the $1.00 cap.
	// The round-2 guard then trips before a second LLM call.
	var llmCalls int
	env.OnActivity(acts.caseActs.CaseLLMStream, mock.Anything, mock.Anything).
		Run(func(mock.Arguments) { llmCalls++ }).
		Return(&CaseLLMStreamResult{Text: "working", CostUSD: 2.0}, nil)

	env.ExecuteWorkflow(NativeSubAgentWorkflow, NativeSubAgentInput{Config: cfg, Input: "go"})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	var out Outcome
	require.NoError(t, env.GetWorkflowResult(&out))
	assert.Equal(t, OutcomeStatusBudgetExceeded, out.Status)
	assert.Equal(t, 1, llmCalls, "the cost-cap guard must stop the loop before a second LLM call")
	assert.InDelta(t, 2.0, out.TotalCostUSD, 1e-9,
		"a native child must surface its metered spend up to the parent's cost cap")
	env.AssertExpectations(t)
}

// TestNativeSubAgentWorkflow_PersistsAcceptedSubmitResult verifies the accepted
// submit_result call/result pair reaches the session transcript (issue #70).
func TestNativeSubAgentWorkflow_PersistsAcceptedSubmitResult(t *testing.T) {
	ts := &testsuite.WorkflowTestSuite{}
	env := ts.NewTestWorkflowEnvironment()
	acts, extActs := registerNativeAgentActivities(env)

	env.OnActivity(extActs.CreateAgentSession, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(acts.chatActs.BuildContext, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(&BuildContextResult{}, nil)
	env.OnActivity(acts.caseActs.CaseLLMStream, mock.Anything, mock.Anything).Return(
		&CaseLLMStreamResult{ToolCalls: []LLMToolCall{nativeSubmitCall("t1-c0", map[string]any{"summary": "done"})}}, nil,
	).Once()
	getPersists := capturePersistRounds(t, env, acts.chatActs.PersistRound)

	env.ExecuteWorkflow(NativeSubAgentWorkflow, NativeSubAgentInput{Config: nativeTestConfig(), Input: "go"})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	assertAcceptedTerminalPersisted(t, getPersists(),
		agentcfg.ReservedToolSubmitResult, "result accepted")
	env.AssertExpectations(t)
}

// TestNativeSubAgentWorkflow_PersistsAcceptedReportFailure verifies the
// accepted report_failure call/result pair reaches the session transcript.
func TestNativeSubAgentWorkflow_PersistsAcceptedReportFailure(t *testing.T) {
	ts := &testsuite.WorkflowTestSuite{}
	env := ts.NewTestWorkflowEnvironment()
	acts, extActs := registerNativeAgentActivities(env)

	env.OnActivity(extActs.CreateAgentSession, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(acts.chatActs.BuildContext, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(&BuildContextResult{}, nil)
	env.OnActivity(acts.caseActs.CaseLLMStream, mock.Anything, mock.Anything).Return(
		&CaseLLMStreamResult{ToolCalls: []LLMToolCall{{
			CallID: "t1-c0", Name: agentcfg.ReservedToolReportFailure,
			Args: map[string]any{argFailReason: "target does not exist"},
		}}}, nil,
	).Once()
	getPersists := capturePersistRounds(t, env, acts.chatActs.PersistRound)

	env.ExecuteWorkflow(NativeSubAgentWorkflow, NativeSubAgentInput{Config: nativeTestConfig(), Input: "go"})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	assertAcceptedTerminalPersisted(t, getPersists(),
		agentcfg.ReservedToolReportFailure, "failure reported")
	env.AssertExpectations(t)
}
