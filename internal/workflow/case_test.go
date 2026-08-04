package workflow

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/testsuite"

	"github.com/tphakala/alfred/internal/agentcfg"
	"github.com/tphakala/alfred/internal/ctxbuild"
	"github.com/tphakala/alfred/internal/store"
)

const (
	testSpawnKindHelper  = "helper"
	testFlakyToolName    = "flaky"
	testFlakyToolCommand = "flaky-cmd"
	testDeclaredToolName = "run_check"
	testEchoCommand      = "/bin/echo"
	testCallDeclared     = "declared"
	testCallFinalize     = "finalize"
)

func testCaseInput() CaseInput {
	return CaseInput{
		Task:         testCaseTask,
		CandidateKey: testCandidateKey,
		Context:      json.RawMessage(`{"title":"hello"}`),
		Config:       agentcfg.AgentConfig{Name: testCaseTask, Strategy: agentcfg.StrategyQueueMonitor},
	}
}

// caseTestActs bundles the activity struct instances a CaseWorkflow test
// needs to register. Registering an activity that a given test never
// exercises is harmless, so most tests register the full set and only mock
// the calls they care about.
type caseTestActs struct {
	monitor  *MonitorActivities
	chatActs *ChatActivities
	caseActs *CaseActivities
}

func registerCaseActivities(env *testsuite.TestWorkflowEnvironment) caseTestActs {
	a := caseTestActs{
		monitor:  &MonitorActivities{},
		chatActs: &ChatActivities{},
		caseActs: &CaseActivities{},
	}
	env.RegisterActivity(a.monitor.ClaimCase)
	env.RegisterActivity(a.monitor.FinalizeCase)
	env.RegisterActivity(a.chatActs.BuildContext)
	env.RegisterActivity(a.chatActs.PersistRound)
	env.RegisterActivity(a.caseActs.CaseLLMStream)
	env.RegisterActivity(a.caseActs.RunDeclaredCommand)
	return a
}

// recordOutcomeCall builds an LLMToolCall for the built-in record_outcome
// tool with the given terminal status.
func recordOutcomeCall(callID, status string) LLMToolCall {
	return LLMToolCall{
		CallID: callID,
		Name:   agentcfg.ReservedToolRecordOutcome,
		Args:   map[string]any{argRecordStatus: status},
	}
}

func TestCaseWorkflow_ClaimFailurePropagatesWithoutFinalize(t *testing.T) {
	ts := &testsuite.WorkflowTestSuite{}
	env := ts.NewTestWorkflowEnvironment()

	a := &MonitorActivities{}
	env.RegisterActivity(a.ClaimCase)
	env.RegisterActivity(a.FinalizeCase)

	env.OnActivity(a.ClaimCase, mock.Anything, mock.Anything).
		Return(assert.AnError)

	env.ExecuteWorkflow(CaseWorkflow, testCaseInput())

	require.True(t, env.IsWorkflowCompleted())
	require.Error(t, env.GetWorkflowError())
	env.AssertNotCalled(t, "FinalizeCase", mock.Anything, mock.Anything)
}

func TestCaseWorkflow_RecordOutcomeDoneRoundOne(t *testing.T) {
	ts := &testsuite.WorkflowTestSuite{}
	env := ts.NewTestWorkflowEnvironment()
	acts := registerCaseActivities(env)

	var calls []string
	env.OnActivity(acts.monitor.ClaimCase, mock.Anything, mock.Anything).
		Run(func(mock.Arguments) { calls = append(calls, "claim") }).
		Return(nil)
	env.OnActivity(acts.chatActs.BuildContext, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(&BuildContextResult{}, nil)
	env.OnActivity(acts.chatActs.PersistRound, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(acts.caseActs.CaseLLMStream, mock.Anything, mock.Anything).
		Run(func(mock.Arguments) { calls = append(calls, "llm") }).
		Return(&CaseLLMStreamResult{
			ToolCalls: []LLMToolCall{recordOutcomeCall("c1", store.TaskStatusDone)},
		}, nil)
	env.OnActivity(acts.monitor.FinalizeCase, mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) {
			calls = append(calls, testCallFinalize)
			// assert (not require) is goroutine-safe here; this callback runs
			// on the activity goroutine, and nothing after ExecuteWorkflow
			// re-checks these args, so fail loudly instead of skipping.
			if finArgs, ok := args.Get(1).(FinalizeCaseArgs); ok {
				assert.Equal(t, store.TaskStatusDone, finArgs.Status)
			} else {
				assert.Fail(t, "unexpected FinalizeCase arg type", "got %T", args.Get(1))
			}
		}).
		Return(nil)

	env.ExecuteWorkflow(CaseWorkflow, testCaseInput())

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	var out CaseOutcome
	require.NoError(t, env.GetWorkflowResult(&out))
	assert.Equal(t, store.TaskStatusDone, out.Status)

	require.Equal(t, []string{"claim", "llm", testCallFinalize}, calls,
		"ClaimCase must run before any LLM call, and finalize only after record_outcome")
}

func TestCaseWorkflow_SpawnAgentThenRecordOutcome(t *testing.T) {
	ts := &testsuite.WorkflowTestSuite{}
	env := ts.NewTestWorkflowEnvironment()
	acts := registerCaseActivities(env)
	env.RegisterWorkflow(ExternalAgentWorkflow)

	subCfg := agentcfg.AgentConfig{Name: testSpawnKindHelper, Strategy: agentcfg.StrategyExternalAgent}
	spawnInput := map[string]any{"note": "check this item"}

	in := testCaseInput()
	in.PromptDir = "prompts/dir"
	in.Config.Supervisor.SubAgents = map[string]agentcfg.AgentConfig{testSpawnKindHelper: subCfg}

	env.OnActivity(acts.monitor.ClaimCase, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(acts.chatActs.BuildContext, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(&BuildContextResult{}, nil)
	env.OnActivity(acts.chatActs.PersistRound, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(acts.monitor.FinalizeCase, mock.Anything, mock.Anything).Return(nil)

	env.OnActivity(acts.caseActs.CaseLLMStream, mock.Anything, mock.Anything).Return(
		&CaseLLMStreamResult{ToolCalls: []LLMToolCall{{
			CallID: "c1", Name: agentcfg.ReservedToolSpawnAgent,
			Args: map[string]any{argSpawnKind: testSpawnKindHelper, argSpawnInput: spawnInput},
		}}}, nil,
	).Once()
	env.OnActivity(acts.caseActs.CaseLLMStream, mock.Anything, mock.Anything).Return(
		&CaseLLMStreamResult{ToolCalls: []LLMToolCall{recordOutcomeCall("c2", store.TaskStatusDone)}}, nil,
	).Once()

	var mu sync.Mutex
	var childInputs []ExternalAgentInput
	env.OnWorkflow(ExternalAgentWorkflow, mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) {
			mu.Lock()
			defer mu.Unlock()
			if in, ok := args.Get(1).(ExternalAgentInput); ok {
				childInputs = append(childInputs, in)
			}
		}).
		Return(Outcome{Status: "ok", TotalCostUSD: 0.5}, nil)

	env.ExecuteWorkflow(CaseWorkflow, in)

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	var out CaseOutcome
	require.NoError(t, env.GetWorkflowResult(&out))
	assert.Equal(t, store.TaskStatusDone, out.Status)

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, childInputs, 1, "exactly one sub-agent child must have been spawned")
	assert.Equal(t, subCfg, childInputs[0].Config)
	assert.Equal(t, spawnInput, childInputs[0].Item)
	assert.Equal(t, uuid.Nil, childInputs[0].SessionID, "a spawned child generates its own session")
	assert.Equal(t, "prompts/dir", childInputs[0].PromptDir)
	// The cli sub-agent must carry its identity so ExecAgentCLI can rehydrate
	// the sub-agent's stripped MCP secrets from the worker's parent config.
	assert.Equal(t, testCaseTask, childInputs[0].SubAgentParent)
	assert.Equal(t, testSpawnKindHelper, childInputs[0].SubAgentKind)
}

func TestCaseWorkflow_SpawnsNativeSubAgent(t *testing.T) {
	ts := &testsuite.WorkflowTestSuite{}
	env := ts.NewTestWorkflowEnvironment()
	acts := registerCaseActivities(env)
	env.RegisterWorkflow(NativeSubAgentWorkflow)

	subCfg := agentcfg.AgentConfig{
		Backend: agentcfg.BackendNative,
		Native: agentcfg.NativeAgent{
			LoopConfig: agentcfg.LoopConfig{
				Model: "m",
			},
			Output: agentcfg.Output{Schema: map[string]any{"type": "object"}},
		},
	}
	spawnInput := map[string]any{"note": "check this item"}

	in := testCaseInput()
	in.PromptDir = "prompts/dir"
	in.Config.Supervisor.SubAgents = map[string]agentcfg.AgentConfig{testSpawnKindHelper: subCfg}

	env.OnActivity(acts.monitor.ClaimCase, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(acts.chatActs.BuildContext, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(&BuildContextResult{}, nil)
	env.OnActivity(acts.chatActs.PersistRound, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(acts.monitor.FinalizeCase, mock.Anything, mock.Anything).Return(nil)

	env.OnActivity(acts.caseActs.CaseLLMStream, mock.Anything, mock.Anything).Return(
		&CaseLLMStreamResult{ToolCalls: []LLMToolCall{{
			CallID: "c1", Name: agentcfg.ReservedToolSpawnAgent,
			Args: map[string]any{argSpawnKind: testSpawnKindHelper, argSpawnInput: spawnInput},
		}}}, nil,
	).Once()
	env.OnActivity(acts.caseActs.CaseLLMStream, mock.Anything, mock.Anything).Return(
		&CaseLLMStreamResult{ToolCalls: []LLMToolCall{recordOutcomeCall("c2", store.TaskStatusDone)}}, nil,
	).Once()

	var mu sync.Mutex
	var childInputs []NativeSubAgentInput
	env.OnWorkflow(NativeSubAgentWorkflow, mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) {
			mu.Lock()
			defer mu.Unlock()
			if in, ok := args.Get(1).(NativeSubAgentInput); ok {
				childInputs = append(childInputs, in)
			}
		}).
		Return(Outcome{Status: "ok", PhaseOutputs: map[string]any{"result": map[string]any{"summary": "done"}}}, nil)

	env.ExecuteWorkflow(CaseWorkflow, in)

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	var out CaseOutcome
	require.NoError(t, env.GetWorkflowResult(&out))
	assert.Equal(t, store.TaskStatusDone, out.Status)

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, childInputs, 1, "exactly one native child must have been spawned")
	assert.Equal(t, subCfg, childInputs[0].Config)
	assert.Equal(t, spawnInput, childInputs[0].Input)
	assert.Equal(t, uuid.Nil, childInputs[0].SessionID, "a spawned child generates its own session")
	assert.Equal(t, "prompts/dir", childInputs[0].PromptDir)
}

func TestNormalizeSpawnOutcome_SurfacesFailureContext(t *testing.T) {
	t.Parallel()
	s := normalizeSpawnOutcome(Outcome{Status: "failed", CancelReason: "max_rounds", FailedAt: "main"})
	var payload map[string]any
	require.NoError(t, json.Unmarshal([]byte(s), &payload))
	assert.Equal(t, "max_rounds", payload["cancel_reason"])
	assert.Equal(t, "main", payload["failed_at"])

	s = normalizeSpawnOutcome(Outcome{Status: "ok"})
	payload = nil
	require.NoError(t, json.Unmarshal([]byte(s), &payload))
	_, hasCancel := payload["cancel_reason"]
	assert.False(t, hasCancel, "cancel_reason must be omitted when empty")
	_, hasFailedAt := payload["failed_at"]
	assert.False(t, hasFailedAt, "failed_at must be omitted when empty")
}

func TestCaseWorkflow_TwoSpawnAgentsInOneRound_BothAwaitedBeforeRoundCompletes(t *testing.T) {
	ts := &testsuite.WorkflowTestSuite{}
	env := ts.NewTestWorkflowEnvironment()
	acts := registerCaseActivities(env)
	env.RegisterWorkflow(ExternalAgentWorkflow)

	in := testCaseInput()
	in.Config.Supervisor.SubAgents = map[string]agentcfg.AgentConfig{
		"a": {Name: "agent-a", Strategy: agentcfg.StrategyExternalAgent},
		"b": {Name: "agent-b", Strategy: agentcfg.StrategyExternalAgent},
	}

	env.OnActivity(acts.monitor.ClaimCase, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(acts.chatActs.BuildContext, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(&BuildContextResult{}, nil)
	env.OnActivity(acts.monitor.FinalizeCase, mock.Anything, mock.Anything).Return(nil)

	getPersists := capturePersistRounds(t, env, acts.chatActs.PersistRound)

	env.OnActivity(acts.caseActs.CaseLLMStream, mock.Anything, mock.Anything).Return(
		&CaseLLMStreamResult{ToolCalls: []LLMToolCall{
			{CallID: "c1", Name: agentcfg.ReservedToolSpawnAgent, Args: map[string]any{argSpawnKind: "a"}},
			{CallID: "c2", Name: agentcfg.ReservedToolSpawnAgent, Args: map[string]any{argSpawnKind: "b"}},
		}}, nil,
	).Once()
	env.OnActivity(acts.caseActs.CaseLLMStream, mock.Anything, mock.Anything).Return(
		&CaseLLMStreamResult{ToolCalls: []LLMToolCall{recordOutcomeCall("c3", store.TaskStatusDone)}}, nil,
	).Once()

	var childMu sync.Mutex
	var childCalls int
	env.OnWorkflow(ExternalAgentWorkflow, mock.Anything, mock.Anything).
		Run(func(mock.Arguments) {
			childMu.Lock()
			defer childMu.Unlock()
			childCalls++
		}).
		Return(Outcome{Status: "ok"}, nil)

	env.ExecuteWorkflow(CaseWorkflow, in)

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	childMu.Lock()
	assert.Equal(t, 2, childCalls, "both spawn_agent calls must start a child")
	childMu.Unlock()

	persistCalls := getPersists()
	var round1 *PersistRoundRequest
	for i := range persistCalls {
		if persistCalls[i].Round == 1 {
			round1 = &persistCalls[i]
			break
		}
	}
	require.NotNil(t, round1, "round 1 must have been persisted")
	assert.Len(t, round1.Messages, 4,
		"both spawn results (tool_call+tool_result pairs) must be in the round 1 persist, "+
			"proving both children were awaited before the round completed")
}

func TestCaseWorkflow_RecordOutcomeAndDeclaredTool_DeclaredRunsBeforeFinalize(t *testing.T) {
	ts := &testsuite.WorkflowTestSuite{}
	env := ts.NewTestWorkflowEnvironment()
	acts := registerCaseActivities(env)

	tool := agentcfg.DeclaredTool{
		Name:    testDeclaredToolName,
		Command: []string{testEchoCommand, "--", "{msg}"},
	}
	in := testCaseInput()
	in.Config.Supervisor.Tools = []agentcfg.DeclaredTool{tool}

	env.OnActivity(acts.monitor.ClaimCase, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(acts.chatActs.BuildContext, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(&BuildContextResult{}, nil)
	env.OnActivity(acts.chatActs.PersistRound, mock.Anything, mock.Anything).Return(nil)

	env.OnActivity(acts.caseActs.CaseLLMStream, mock.Anything, mock.Anything).Return(
		&CaseLLMStreamResult{ToolCalls: []LLMToolCall{
			{CallID: "c1", Name: testDeclaredToolName, Args: map[string]any{"msg": "hi"}},
			recordOutcomeCall("c2", store.TaskStatusDone),
		}}, nil,
	)

	var calls []string
	env.OnActivity(acts.caseActs.RunDeclaredCommand, mock.Anything, mock.Anything).
		Run(func(mock.Arguments) { calls = append(calls, testCallDeclared) }).
		Return(RunDeclaredCommandResult{ExitCode: 0, Stdout: "hi\n"}, nil)
	env.OnActivity(acts.monitor.FinalizeCase, mock.Anything, mock.Anything).
		Run(func(mock.Arguments) { calls = append(calls, testCallFinalize) }).
		Return(nil)

	env.ExecuteWorkflow(CaseWorkflow, in)

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	require.Equal(t, []string{testCallDeclared, testCallFinalize}, calls,
		"the declared tool call must run before the round finalizes, even though record_outcome "+
			"was in the same round")
}

// TestCaseWorkflow_RecordOutcomeEmittedFirst_DeclaredToolStillRuns locks in
// that classifyToolCalls buckets calls by kind, not by position: a
// terminal record_outcome emitted first in the tool-call slice must not
// short-circuit a non-terminal declared-tool call emitted after it. The
// declared tool must still run, and FinalizeCase must still run only after
// it, regardless of emission order.
func TestCaseWorkflow_RecordOutcomeEmittedFirst_DeclaredToolStillRuns(t *testing.T) {
	ts := &testsuite.WorkflowTestSuite{}
	env := ts.NewTestWorkflowEnvironment()
	acts := registerCaseActivities(env)

	tool := agentcfg.DeclaredTool{
		Name:    testDeclaredToolName,
		Command: []string{testEchoCommand, "--", "{msg}"},
	}
	in := testCaseInput()
	in.Config.Supervisor.Tools = []agentcfg.DeclaredTool{tool}

	env.OnActivity(acts.monitor.ClaimCase, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(acts.chatActs.BuildContext, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(&BuildContextResult{}, nil)
	env.OnActivity(acts.chatActs.PersistRound, mock.Anything, mock.Anything).Return(nil)

	env.OnActivity(acts.caseActs.CaseLLMStream, mock.Anything, mock.Anything).Return(
		&CaseLLMStreamResult{ToolCalls: []LLMToolCall{
			recordOutcomeCall("c1", store.TaskStatusDone),
			{CallID: "c2", Name: testDeclaredToolName, Args: map[string]any{"msg": "hi"}},
		}}, nil,
	)

	var calls []string
	env.OnActivity(acts.caseActs.RunDeclaredCommand, mock.Anything, mock.Anything).
		Run(func(mock.Arguments) { calls = append(calls, testCallDeclared) }).
		Return(RunDeclaredCommandResult{ExitCode: 0, Stdout: "hi\n"}, nil)
	env.OnActivity(acts.monitor.FinalizeCase, mock.Anything, mock.Anything).
		Run(func(mock.Arguments) { calls = append(calls, testCallFinalize) }).
		Return(nil)

	env.ExecuteWorkflow(CaseWorkflow, in)

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	require.Equal(t, []string{testCallDeclared, testCallFinalize}, calls,
		"the declared tool call must still run, and finalize must still run after it, even though "+
			"record_outcome was emitted first in the tool-call slice")
}

// runDeclaredToolRetryCase drives one CaseWorkflow round where a declared
// tool always fails, then asserts how many times RunDeclaredCommand ran for
// the given idempotency setting. Shared by the non-idempotent and idempotent
// variants below: the two scenarios differ only in the tool's Idempotent
// flag and the expected attempt count, so factoring the mock wiring out
// keeps them from being near-identical copies of each other.
func runDeclaredToolRetryCase(t *testing.T, idempotent bool, wantAttempts int, msg string) {
	t.Helper()
	ts := &testsuite.WorkflowTestSuite{}
	env := ts.NewTestWorkflowEnvironment()
	acts := registerCaseActivities(env)

	tool := agentcfg.DeclaredTool{
		Name:       testFlakyToolName,
		Command:    []string{testFlakyToolCommand, "--"},
		Idempotent: idempotent,
	}
	in := testCaseInput()
	in.Config.Supervisor.Tools = []agentcfg.DeclaredTool{tool}

	env.OnActivity(acts.monitor.ClaimCase, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(acts.chatActs.BuildContext, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(&BuildContextResult{}, nil)
	env.OnActivity(acts.chatActs.PersistRound, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(acts.monitor.FinalizeCase, mock.Anything, mock.Anything).Return(nil)

	env.OnActivity(acts.caseActs.CaseLLMStream, mock.Anything, mock.Anything).Return(
		&CaseLLMStreamResult{ToolCalls: []LLMToolCall{
			{CallID: "c1", Name: testFlakyToolName, Args: map[string]any{}},
		}}, nil,
	).Once()
	env.OnActivity(acts.caseActs.CaseLLMStream, mock.Anything, mock.Anything).Return(
		&CaseLLMStreamResult{ToolCalls: []LLMToolCall{recordOutcomeCall("c2", store.TaskStatusDone)}}, nil,
	).Once()

	var mu sync.Mutex
	attempts := 0
	env.OnActivity(acts.caseActs.RunDeclaredCommand, mock.Anything, mock.Anything).
		Run(func(mock.Arguments) {
			mu.Lock()
			defer mu.Unlock()
			attempts++
		}).
		Return(RunDeclaredCommandResult{}, fmt.Errorf("command failed"))

	env.ExecuteWorkflow(CaseWorkflow, in)

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, wantAttempts, attempts, msg)
}

func TestCaseWorkflow_DeclaredToolNonIdempotent_NotRetried(t *testing.T) {
	runDeclaredToolRetryCase(t, false, declaredToolSingleAttempt,
		"a non-idempotent declared tool must not be retried")
}

func TestCaseWorkflow_DeclaredToolIdempotent_Retries(t *testing.T) {
	runDeclaredToolRetryCase(t, true, declaredToolIdempotentAttempts,
		"an idempotent declared tool must be retried up to the configured attempt count")
}

func TestCaseWorkflow_MaxRoundsExhausted_NeedsAttention(t *testing.T) {
	ts := &testsuite.WorkflowTestSuite{}
	env := ts.NewTestWorkflowEnvironment()
	acts := registerCaseActivities(env)

	in := testCaseInput()
	in.Config.Supervisor.MaxRounds = 2

	env.OnActivity(acts.monitor.ClaimCase, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(acts.chatActs.BuildContext, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(&BuildContextResult{}, nil)
	env.OnActivity(acts.chatActs.PersistRound, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(acts.caseActs.CaseLLMStream, mock.Anything, mock.Anything).Return(
		&CaseLLMStreamResult{Text: "still working on it"}, nil,
	)

	getFinArgs := captureFinalizeCase(t, env, acts.monitor.FinalizeCase)

	env.ExecuteWorkflow(CaseWorkflow, in)

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	finArgs := getFinArgs()
	assert.Equal(t, store.TaskStatusNeedsAttention, finArgs.Status)
	assertOutcomeReason(t, finArgs.Outcome, guardMaxRounds)
}

func TestCaseWorkflow_CostCapExceeded_NeedsAttention(t *testing.T) {
	ts := &testsuite.WorkflowTestSuite{}
	env := ts.NewTestWorkflowEnvironment()
	acts := registerCaseActivities(env)
	env.RegisterWorkflow(ExternalAgentWorkflow)

	in := testCaseInput()
	in.Config.Supervisor.CostCapUSD = 1.0
	in.Config.Supervisor.SubAgents = map[string]agentcfg.AgentConfig{
		testSpawnKindHelper: {Name: testSpawnKindHelper, Strategy: agentcfg.StrategyExternalAgent},
	}

	env.OnActivity(acts.monitor.ClaimCase, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(acts.chatActs.BuildContext, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(&BuildContextResult{}, nil)
	env.OnActivity(acts.chatActs.PersistRound, mock.Anything, mock.Anything).Return(nil)
	env.OnWorkflow(ExternalAgentWorkflow, mock.Anything, mock.Anything).
		Return(Outcome{Status: "ok", TotalCostUSD: 2.0}, nil)

	var llmCalls int
	env.OnActivity(acts.caseActs.CaseLLMStream, mock.Anything, mock.Anything).
		Run(func(mock.Arguments) { llmCalls++ }).
		Return(&CaseLLMStreamResult{ToolCalls: []LLMToolCall{
			{CallID: "c1", Name: agentcfg.ReservedToolSpawnAgent, Args: map[string]any{argSpawnKind: testSpawnKindHelper}},
		}}, nil).Once()

	getFinArgs := captureFinalizeCase(t, env, acts.monitor.FinalizeCase)

	env.ExecuteWorkflow(CaseWorkflow, in)

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	assert.Equal(t, 1, llmCalls, "the cost-cap guard must stop the loop before a second LLM call")
	finArgs := getFinArgs()
	assert.Equal(t, store.TaskStatusNeedsAttention, finArgs.Status)
	assertOutcomeReason(t, finArgs.Outcome, guardCostCap)
}

// TestCaseWorkflow_SupervisorLLMSpendTripsCostCap covers #66's headline case: a
// supervisor that spawns nothing but runs expensive LLM rounds. Before token
// metering, its own spend was unpriced, so cost_cap was inert for this path and
// only max_rounds/max_duration bounded it. Now the metered LLM cost trips it.
func TestCaseWorkflow_SupervisorLLMSpendTripsCostCap(t *testing.T) {
	ts := &testsuite.WorkflowTestSuite{}
	env := ts.NewTestWorkflowEnvironment()
	acts := registerCaseActivities(env)

	in := testCaseInput()
	in.Config.Supervisor.CostCapUSD = 1.0 // no SubAgents: the only spend is the supervisor's own LLM rounds

	env.OnActivity(acts.monitor.ClaimCase, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(acts.chatActs.BuildContext, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(&BuildContextResult{}, nil)
	env.OnActivity(acts.chatActs.PersistRound, mock.Anything, mock.Anything).Return(nil)

	// Each round is non-terminal but reports $2.00 of LLM spend, exceeding the
	// $1.00 cap, so the round-2 guard trips before a second LLM call.
	var llmCalls int
	env.OnActivity(acts.caseActs.CaseLLMStream, mock.Anything, mock.Anything).
		Run(func(mock.Arguments) { llmCalls++ }).
		Return(&CaseLLMStreamResult{Text: "thinking out loud", CostUSD: 2.0}, nil)

	getFinArgs := captureFinalizeCase(t, env, acts.monitor.FinalizeCase)

	env.ExecuteWorkflow(CaseWorkflow, in)

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	assert.Equal(t, 1, llmCalls, "the supervisor's own metered spend must trip cost_cap before a second LLM call")
	finArgs := getFinArgs()
	assert.Equal(t, store.TaskStatusNeedsAttention, finArgs.Status)
	assertOutcomeReason(t, finArgs.Outcome, guardCostCap)
}

func TestCaseWorkflow_MaxDurationExceeded_NeedsAttention(t *testing.T) {
	ts := &testsuite.WorkflowTestSuite{}
	env := ts.NewTestWorkflowEnvironment()
	acts := registerCaseActivities(env)

	in := testCaseInput()
	in.Config.Supervisor.MaxDuration = 5 * time.Minute

	env.OnActivity(acts.monitor.ClaimCase, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(acts.chatActs.BuildContext, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(&BuildContextResult{}, nil)
	env.OnActivity(acts.chatActs.PersistRound, mock.Anything, mock.Anything).Return(nil)

	var llmCalls int
	env.OnActivity(acts.caseActs.CaseLLMStream, mock.Anything, mock.Anything).
		Run(func(mock.Arguments) { llmCalls++ }).
		Return(&CaseLLMStreamResult{Text: "working"}, nil).
		Once().
		After(10 * time.Minute)

	getFinArgs := captureFinalizeCase(t, env, acts.monitor.FinalizeCase)

	env.ExecuteWorkflow(CaseWorkflow, in)

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	assert.Equal(t, 1, llmCalls, "the max-duration guard must stop the loop before a second LLM call")
	finArgs := getFinArgs()
	assert.Equal(t, store.TaskStatusNeedsAttention, finArgs.Status)
	assertOutcomeReason(t, finArgs.Outcome, guardMaxDuration)
}

func TestCaseWorkflow_InvalidRecordOutcomeStatus_NotFinalized(t *testing.T) {
	ts := &testsuite.WorkflowTestSuite{}
	env := ts.NewTestWorkflowEnvironment()
	acts := registerCaseActivities(env)

	in := testCaseInput()
	in.Config.Supervisor.MaxRounds = 1

	env.OnActivity(acts.monitor.ClaimCase, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(acts.chatActs.BuildContext, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(&BuildContextResult{}, nil)
	env.OnActivity(acts.caseActs.CaseLLMStream, mock.Anything, mock.Anything).Return(
		&CaseLLMStreamResult{ToolCalls: []LLMToolCall{recordOutcomeCall("c1", "bogus_status")}}, nil,
	)

	getPersists := capturePersistRounds(t, env, acts.chatActs.PersistRound)

	var finCalls int
	var finArgs FinalizeCaseArgs
	env.OnActivity(acts.monitor.FinalizeCase, mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) {
			finCalls++
			if v, ok := args.Get(1).(FinalizeCaseArgs); ok {
				finArgs = v
			}
		}).
		Return(nil)

	env.ExecuteWorkflow(CaseWorkflow, in)

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	// The invalid status must not finalize the case directly: it only ends
	// up needs_attention once max_rounds is exhausted afterward.
	assert.Equal(t, 1, finCalls, "FinalizeCase must run exactly once, from the max_rounds guard")
	assert.Equal(t, store.TaskStatusNeedsAttention, finArgs.Status)
	assertOutcomeReason(t, finArgs.Outcome, guardMaxRounds)

	assert.True(t, persistedToolResultContains(getPersists(), "invalid record_outcome status"),
		"an error tool result explaining the invalid status must have been persisted")
}

// assertOutcomeReason unmarshals a FinalizeCase outcome payload and asserts
// its "reason" field.
func assertOutcomeReason(t *testing.T, outcome json.RawMessage, wantReason string) {
	t.Helper()
	var payload struct {
		Reason string `json:"reason"`
	}
	require.NoError(t, json.Unmarshal(outcome, &payload))
	assert.Equal(t, wantReason, payload.Reason)
}

// capturePersistRounds registers a PersistRound mock that records every
// request, and returns a getter that safely copies the collected requests
// after the workflow completes (activities may run off the workflow goroutine
// in the test env, hence the mutex). Shared by every test that asserts on persisted rounds.
func capturePersistRounds(t *testing.T, env *testsuite.TestWorkflowEnvironment, persist any) func() []PersistRoundRequest {
	t.Helper()
	var mu sync.Mutex
	var calls []PersistRoundRequest
	env.OnActivity(persist, mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) {
			mu.Lock()
			defer mu.Unlock()
			req, ok := args.Get(1).(PersistRoundRequest)
			// assert (not require) is goroutine-safe here: this callback runs on
			// the activity goroutine, and t.Errorf does not call Goexit. A wrong
			// arg type is a mock/signature bug, so surface it with the got type
			// instead of silently dropping the call.
			if !assert.True(t, ok, "PersistRound arg 1: want PersistRoundRequest, got %T", args.Get(1)) {
				return
			}
			calls = append(calls, req)
		}).
		Return(nil)
	return func() []PersistRoundRequest {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(calls)
	}
}

// captureFinalizeCase registers a FinalizeCase mock (expected once) that
// records the FinalizeCaseArgs it receives and returns a getter that reads it
// after the workflow completes. Mirrors capturePersistRounds; shared by the
// guard tests that assert on the finalize status and outcome.
func captureFinalizeCase(t *testing.T, env *testsuite.TestWorkflowEnvironment, finalize any) func() FinalizeCaseArgs {
	t.Helper()
	var mu sync.Mutex
	var finArgs FinalizeCaseArgs
	env.OnActivity(finalize, mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) {
			mu.Lock()
			defer mu.Unlock()
			v, ok := args.Get(1).(FinalizeCaseArgs)
			if !assert.True(t, ok, "FinalizeCase arg 1: want FinalizeCaseArgs, got %T", args.Get(1)) {
				return
			}
			finArgs = v
		}).
		Return(nil).Once()
	return func() FinalizeCaseArgs {
		mu.Lock()
		defer mu.Unlock()
		return finArgs
	}
}

// assertAcceptedTerminalPersisted asserts that persistCalls contains the
// accepted terminal call/result pair: a PersistRound under an "-accepted"
// idempotency key holding the tool_call for toolName and a tool_result whose
// content confirms acceptance (resultSubstr).
func assertAcceptedTerminalPersisted(t *testing.T, persistCalls []PersistRoundRequest, toolName, resultSubstr string) {
	t.Helper()
	var accepted *PersistRoundRequest
	for i := range persistCalls {
		if strings.HasSuffix(persistCalls[i].IdempotencyKey, "-accepted") {
			accepted = &persistCalls[i]
		}
	}
	require.NotNil(t, accepted, "the accepted terminal call must be persisted under an \"-accepted\" key")
	var sawCall, sawResult bool
	for _, m := range accepted.Messages {
		if m.Role == ctxbuild.RoleToolCall && m.Content == toolName {
			sawCall = true
		}
		if m.Role == ctxbuild.RoleToolResult && strings.Contains(m.Content, resultSubstr) {
			sawResult = true
		}
	}
	assert.True(t, sawCall, "the accepted persist must include the %s tool_call", toolName)
	assert.True(t, sawResult, "the accepted persist must include a tool_result confirming acceptance")
}

// persistedToolResultContains reports whether any persisted round holds a
// tool_result message whose content contains every one of substrs. Shared by
// tests that assert an error or rejection explanation reached the transcript.
func persistedToolResultContains(calls []PersistRoundRequest, substrs ...string) bool {
	for _, req := range calls {
		for _, m := range req.Messages {
			if m.Role != ctxbuild.RoleToolResult {
				continue
			}
			all := true
			for _, sub := range substrs {
				if !strings.Contains(m.Content, sub) {
					all = false
					break
				}
			}
			if all {
				return true
			}
		}
	}
	return false
}

// TestCaseWorkflow_PersistsAcceptedRecordOutcome verifies the accepted
// record_outcome call/result pair reaches the session transcript (issue #70):
// previously only the durable task_state ledger recorded the terminal outcome.
func TestCaseWorkflow_PersistsAcceptedRecordOutcome(t *testing.T) {
	ts := &testsuite.WorkflowTestSuite{}
	env := ts.NewTestWorkflowEnvironment()
	acts := registerCaseActivities(env)

	env.OnActivity(acts.monitor.ClaimCase, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(acts.chatActs.BuildContext, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(&BuildContextResult{}, nil)
	env.OnActivity(acts.caseActs.CaseLLMStream, mock.Anything, mock.Anything).Return(
		&CaseLLMStreamResult{ToolCalls: []LLMToolCall{recordOutcomeCall("c1", store.TaskStatusDone)}}, nil,
	)
	env.OnActivity(acts.monitor.FinalizeCase, mock.Anything, mock.Anything).Return(nil)
	getPersists := capturePersistRounds(t, env, acts.chatActs.PersistRound)

	env.ExecuteWorkflow(CaseWorkflow, testCaseInput())

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	assertAcceptedTerminalPersisted(t, getPersists(),
		agentcfg.ReservedToolRecordOutcome, "recorded outcome: "+store.TaskStatusDone)
	env.AssertExpectations(t)
}
