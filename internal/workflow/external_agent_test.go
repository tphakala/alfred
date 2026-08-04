package workflow

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"github.com/tphakala/alfred/internal/agent/runner"
	"github.com/tphakala/alfred/internal/agentcfg"
	"go.temporal.io/sdk/testsuite"
)

// captureHookCommands registers a RunStateHook mock that records the Command
// of every state hook it runs, in order, and returns a getter read after the
// workflow completes. Mirrors capturePersistRounds; shared by the OnComplete
// hook tests.
func captureHookCommands(t *testing.T, env *testsuite.TestWorkflowEnvironment, runHook any) func() []string {
	t.Helper()
	var mu sync.Mutex
	var cmds []string
	env.OnActivity(runHook, mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) {
			mu.Lock()
			defer mu.Unlock()
			hook, ok := args.Get(1).(agentcfg.StateHook)
			if !assert.True(t, ok, "RunStateHook arg 1: want agentcfg.StateHook, got %T", args.Get(1)) {
				return
			}
			cmds = append(cmds, hook.Command)
		}).Return(nil)
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(cmds)
	}
}

// writeTestPrompt creates a temporary prompt file and returns the root
// directory so that PreparePhaseInput can resolve "prompts/agents/x.md".
func writeTestPrompt(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	promptDir := filepath.Join(dir, "prompts", "agents")
	require.NoError(t, os.MkdirAll(promptDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(promptDir, "x.md"), []byte("test prompt"), 0o644))
	return dir
}

// testExternalAgentConfig returns a minimal AgentConfig suitable for workflow
// tests, with PromptDir pointing at a temp directory containing the prompt file.
func testExternalAgentConfig() agentcfg.AgentConfig {
	return agentcfg.AgentConfig{
		Name:     "x",
		Strategy: "external_agent",
		Runner:   "claude",
		Prefilter: agentcfg.Prefilter{
			Type:    "command",
			Command: "echo {}",
		},
		Phases: []agentcfg.Phase{{
			Name:  "main",
			Model: "claude-sonnet-4-6",
			Prompt: agentcfg.Prompt{
				Base: "prompts/agents/x.md",
			},
			Steering: agentcfg.Steering{Mode: agentcfg.SteeringScheduled},
			Output:   agentcfg.Output{Capture: agentcfg.OutputCaptureText},
		}},
	}
}

func TestExternalAgentWorkflow_NoWorkExitsCleanly(t *testing.T) {
	ts := &testsuite.WorkflowTestSuite{}
	env := ts.NewTestWorkflowEnvironment()

	a := &ExternalAgentActivities{}
	env.RegisterActivity(a.RunPreflight)
	env.RegisterActivity(a.RunPrefilter)
	env.RegisterActivity(a.PreparePhaseInput)
	env.RegisterActivity(a.ExecAgentCLI)
	env.RegisterActivity(a.RunCleanup)
	env.RegisterActivity(a.CreateAgentSession)

	env.OnActivity(a.CreateAgentSession, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.RunPrefilter, mock.Anything, mock.Anything).
		Return(PrefilterResult{Proceed: false}, nil)

	env.ExecuteWorkflow(ExternalAgentWorkflow, ExternalAgentInput{
		SessionID: uuid.New(),
		PromptDir: writeTestPrompt(t),
		Config:    testExternalAgentConfig(),
	})

	require.True(t, env.IsWorkflowCompleted())
	var out Outcome
	require.NoError(t, env.GetWorkflowResult(&out))
	require.Equal(t, "no_work", out.Status)
}

func TestExternalAgentWorkflow_HappyPath(t *testing.T) {
	ts := &testsuite.WorkflowTestSuite{}
	env := ts.NewTestWorkflowEnvironment()

	a := &ExternalAgentActivities{}
	env.RegisterActivity(a.RunPreflight)
	env.RegisterActivity(a.RunPrefilter)
	env.RegisterActivity(a.PreparePhaseInput)
	env.RegisterActivity(a.ExecAgentCLI)
	env.RegisterActivity(a.RunCleanup)
	env.RegisterActivity(a.CreateAgentSession)

	env.OnActivity(a.CreateAgentSession, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.RunPrefilter, mock.Anything, mock.Anything).
		Return(PrefilterResult{Proceed: true, Data: map[string]any{"foo": 1}}, nil)
	// PreparePhaseInput runs as a real activity (not mocked) to preserve
	// end-to-end prompt rendering coverage.
	env.OnActivity(a.ExecAgentCLI, mock.Anything, mock.Anything).
		Return(ExecOutput{ExitCode: 0, FinalText: "done"}, nil)

	env.ExecuteWorkflow(ExternalAgentWorkflow, ExternalAgentInput{
		SessionID: uuid.New(),
		PromptDir: writeTestPrompt(t),
		Config:    testExternalAgentConfig(),
	})

	require.True(t, env.IsWorkflowCompleted())
	var out Outcome
	require.NoError(t, env.GetWorkflowResult(&out))
	require.Equal(t, "ok", out.Status)
}

func TestExternalAgentWorkflow_BudgetExceeded(t *testing.T) {
	ts := &testsuite.WorkflowTestSuite{}
	env := ts.NewTestWorkflowEnvironment()

	a := &ExternalAgentActivities{}
	env.RegisterActivity(a.RunPreflight)
	env.RegisterActivity(a.RunPrefilter)
	env.RegisterActivity(a.PreparePhaseInput)
	env.RegisterActivity(a.ExecAgentCLI)
	env.RegisterActivity(a.RunCleanup)
	env.RegisterActivity(a.CreateAgentSession)
	env.RegisterActivity(a.RunStateHook)

	cfg := testExternalAgentConfig()
	cfg.Budgets = agentcfg.Budgets{WorkflowMaxCostUSD: 0.50}

	env.OnActivity(a.CreateAgentSession, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.RunPrefilter, mock.Anything, mock.Anything).
		Return(PrefilterResult{Proceed: true}, nil)
	env.OnActivity(a.ExecAgentCLI, mock.Anything, mock.Anything).
		Return(ExecOutput{ExitCode: 0, TotalCostUSD: 0.75, FinalText: "done"}, nil)

	env.ExecuteWorkflow(ExternalAgentWorkflow, ExternalAgentInput{
		SessionID: uuid.New(),
		PromptDir: writeTestPrompt(t),
		Config:    cfg,
	})

	require.True(t, env.IsWorkflowCompleted())
	var out Outcome
	require.NoError(t, env.GetWorkflowResult(&out))
	require.Equal(t, "budget_exceeded", out.Status)
	require.Equal(t, "main", out.FailedAt)
	require.InDelta(t, 0.75, out.TotalCostUSD, 0.001)
}

func TestExternalAgentWorkflow_BudgetAccumulatesAcrossPhases(t *testing.T) {
	ts := &testsuite.WorkflowTestSuite{}
	env := ts.NewTestWorkflowEnvironment()

	a := &ExternalAgentActivities{}
	env.RegisterActivity(a.RunPreflight)
	env.RegisterActivity(a.RunPrefilter)
	env.RegisterActivity(a.PreparePhaseInput)
	env.RegisterActivity(a.ExecAgentCLI)
	env.RegisterActivity(a.RunCleanup)
	env.RegisterActivity(a.CreateAgentSession)
	env.RegisterActivity(a.RunStateHook)

	cfg := agentcfg.AgentConfig{
		Name:     "x",
		Strategy: "external_agent",
		Runner:   "claude",
		Budgets:  agentcfg.Budgets{WorkflowMaxCostUSD: 1.00},
		Phases: []agentcfg.Phase{
			{Name: "phase1", Model: "m", Prompt: agentcfg.Prompt{Base: "prompts/agents/x.md"}, Steering: agentcfg.Steering{Mode: agentcfg.SteeringScheduled}, Output: agentcfg.Output{Capture: agentcfg.OutputCaptureText}},
			{Name: "phase2", Model: "m", Prompt: agentcfg.Prompt{Base: "prompts/agents/x.md"}, Steering: agentcfg.Steering{Mode: agentcfg.SteeringScheduled}, Output: agentcfg.Output{Capture: agentcfg.OutputCaptureText}},
			{Name: "phase3", Model: "m", Prompt: agentcfg.Prompt{Base: "prompts/agents/x.md"}, Steering: agentcfg.Steering{Mode: agentcfg.SteeringScheduled}, Output: agentcfg.Output{Capture: agentcfg.OutputCaptureText}},
		},
	}

	env.OnActivity(a.CreateAgentSession, mock.Anything, mock.Anything).Return(nil)
	callCount := 0
	env.OnActivity(a.ExecAgentCLI, mock.Anything, mock.Anything).
		Run(func(_ mock.Arguments) { callCount++ }).
		Return(ExecOutput{ExitCode: 0, TotalCostUSD: 0.40, FinalText: "done"}, nil)

	env.ExecuteWorkflow(ExternalAgentWorkflow, ExternalAgentInput{
		SessionID: uuid.New(),
		PromptDir: writeTestPrompt(t),
		Config:    cfg,
	})

	require.True(t, env.IsWorkflowCompleted())
	var out Outcome
	require.NoError(t, env.GetWorkflowResult(&out))
	require.Equal(t, "budget_exceeded", out.Status,
		"0.40 + 0.40 + 0.40 = 1.20 > 1.00 budget")
	require.Equal(t, 3, callCount, "all three phases should execute before budget check trips on phase3")
	require.InDelta(t, 1.20, out.TotalCostUSD, 0.001)
}

func TestExternalAgentWorkflow_NoBudgetSkipsCheck(t *testing.T) {
	ts := &testsuite.WorkflowTestSuite{}
	env := ts.NewTestWorkflowEnvironment()

	a := &ExternalAgentActivities{}
	env.RegisterActivity(a.RunPreflight)
	env.RegisterActivity(a.RunPrefilter)
	env.RegisterActivity(a.PreparePhaseInput)
	env.RegisterActivity(a.ExecAgentCLI)
	env.RegisterActivity(a.RunCleanup)
	env.RegisterActivity(a.CreateAgentSession)
	env.RegisterActivity(a.RunStateHook)

	cfg := testExternalAgentConfig()
	// WorkflowMaxCostUSD is zero (default), so no budget check.

	env.OnActivity(a.CreateAgentSession, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.RunPrefilter, mock.Anything, mock.Anything).
		Return(PrefilterResult{Proceed: true}, nil)
	env.OnActivity(a.ExecAgentCLI, mock.Anything, mock.Anything).
		Return(ExecOutput{ExitCode: 0, TotalCostUSD: 999.99, FinalText: "done"}, nil)

	env.ExecuteWorkflow(ExternalAgentWorkflow, ExternalAgentInput{
		SessionID: uuid.New(),
		PromptDir: writeTestPrompt(t),
		Config:    cfg,
	})

	require.True(t, env.IsWorkflowCompleted())
	var out Outcome
	require.NoError(t, env.GetWorkflowResult(&out))
	require.Equal(t, "ok", out.Status, "no budget cap means no limit")
	require.InDelta(t, 999.99, out.TotalCostUSD, 0.001)
}

func TestExternalAgentWorkflow_OnCompleteHooksOnSuccess(t *testing.T) {
	ts := &testsuite.WorkflowTestSuite{}
	env := ts.NewTestWorkflowEnvironment()

	a := &ExternalAgentActivities{}
	env.RegisterActivity(a.RunPreflight)
	env.RegisterActivity(a.RunPrefilter)
	env.RegisterActivity(a.PreparePhaseInput)
	env.RegisterActivity(a.ExecAgentCLI)
	env.RegisterActivity(a.RunCleanup)
	env.RegisterActivity(a.CreateAgentSession)
	env.RegisterActivity(a.RunStateHook)

	cfg := testExternalAgentConfig()
	cfg.State = agentcfg.State{
		OnComplete: []agentcfg.StateHook{
			{Type: "bash", Command: "echo success-hook"},
			{Type: "bash", Command: "echo always-hook", When: "always"},
		},
	}

	env.OnActivity(a.CreateAgentSession, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.RunPrefilter, mock.Anything, mock.Anything).
		Return(PrefilterResult{Proceed: true}, nil)
	env.OnActivity(a.ExecAgentCLI, mock.Anything, mock.Anything).
		Return(ExecOutput{ExitCode: 0, FinalText: "done"}, nil)

	getHookCalls := captureHookCommands(t, env, a.RunStateHook)

	env.ExecuteWorkflow(ExternalAgentWorkflow, ExternalAgentInput{
		SessionID: uuid.New(),
		PromptDir: writeTestPrompt(t),
		Config:    cfg,
	})

	require.True(t, env.IsWorkflowCompleted())
	var out Outcome
	require.NoError(t, env.GetWorkflowResult(&out))
	require.Equal(t, "ok", out.Status)
	require.Equal(t, []string{"echo success-hook", "echo always-hook"}, getHookCalls(),
		"both success and always hooks must run on success")
}

func TestExternalAgentWorkflow_OnCompleteHooksOnFailure(t *testing.T) {
	ts := &testsuite.WorkflowTestSuite{}
	env := ts.NewTestWorkflowEnvironment()

	a := &ExternalAgentActivities{}
	env.RegisterActivity(a.RunPreflight)
	env.RegisterActivity(a.RunPrefilter)
	env.RegisterActivity(a.PreparePhaseInput)
	env.RegisterActivity(a.ExecAgentCLI)
	env.RegisterActivity(a.RunCleanup)
	env.RegisterActivity(a.CreateAgentSession)
	env.RegisterActivity(a.RunStateHook)

	cfg := testExternalAgentConfig()
	cfg.State = agentcfg.State{
		OnComplete: []agentcfg.StateHook{
			{Type: "bash", Command: "echo success-hook"},
			{Type: "bash", Command: "echo always-hook", When: "always"},
		},
	}

	env.OnActivity(a.CreateAgentSession, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.RunPrefilter, mock.Anything, mock.Anything).
		Return(PrefilterResult{Proceed: true}, nil)
	env.OnActivity(a.PreparePhaseInput, mock.Anything, mock.Anything).
		Return(ExecInput{}, fmt.Errorf("prompt error"))

	getHookCalls := captureHookCommands(t, env, a.RunStateHook)

	env.ExecuteWorkflow(ExternalAgentWorkflow, ExternalAgentInput{
		SessionID: uuid.New(),
		PromptDir: writeTestPrompt(t),
		Config:    cfg,
	})

	require.True(t, env.IsWorkflowCompleted())
	require.Error(t, env.GetWorkflowError())
	require.Equal(t, []string{"echo always-hook"}, getHookCalls(),
		"only 'always' hooks must run on failure, not success hooks")
}

func TestExternalAgentWorkflow_StateBankPassedToPrepare(t *testing.T) {
	ts := &testsuite.WorkflowTestSuite{}
	env := ts.NewTestWorkflowEnvironment()

	a := &ExternalAgentActivities{}
	env.RegisterActivity(a.RunPreflight)
	env.RegisterActivity(a.RunPrefilter)
	env.RegisterActivity(a.PreparePhaseInput)
	env.RegisterActivity(a.ExecAgentCLI)
	env.RegisterActivity(a.RunCleanup)
	env.RegisterActivity(a.CreateAgentSession)
	env.RegisterActivity(a.RunStateHook)

	cfg := testExternalAgentConfig()
	cfg.State = agentcfg.State{Bank: "my-bank"}

	env.OnActivity(a.CreateAgentSession, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.RunPrefilter, mock.Anything, mock.Anything).
		Return(PrefilterResult{Proceed: true}, nil)

	var capturedBank string
	env.OnActivity(a.PreparePhaseInput, mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) {
			pArgs := args.Get(1).(PreparePhaseInputArgs)
			capturedBank = pArgs.StateBank
		}).Return(ExecInput{
		SessionID: uuid.New(),
		Runner:    "claude",
		Phase:     "main",
		Config:    runner.RunConfig{Model: "m", Prompt: "p"},
	}, nil)
	env.OnActivity(a.ExecAgentCLI, mock.Anything, mock.Anything).
		Return(ExecOutput{ExitCode: 0}, nil)

	env.ExecuteWorkflow(ExternalAgentWorkflow, ExternalAgentInput{
		SessionID: uuid.New(),
		PromptDir: writeTestPrompt(t),
		Config:    cfg,
	})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	require.Equal(t, "my-bank", capturedBank,
		"State.Bank must be forwarded as StateBank in PreparePhaseInputArgs")
}

func TestExternalAgentWorkflow_PreparePhaseInputFails(t *testing.T) {
	ts := &testsuite.WorkflowTestSuite{}
	env := ts.NewTestWorkflowEnvironment()

	a := &ExternalAgentActivities{}
	env.RegisterActivity(a.RunPreflight)
	env.RegisterActivity(a.RunPrefilter)
	env.RegisterActivity(a.PreparePhaseInput)
	env.RegisterActivity(a.ExecAgentCLI)
	env.RegisterActivity(a.RunCleanup)
	env.RegisterActivity(a.CreateAgentSession)

	env.OnActivity(a.CreateAgentSession, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.RunPrefilter, mock.Anything, mock.Anything).
		Return(PrefilterResult{Proceed: true, Data: map[string]any{"foo": 1}}, nil)
	env.OnActivity(a.PreparePhaseInput, mock.Anything, mock.Anything).
		Return(ExecInput{}, fmt.Errorf("prompt file not found"))

	env.ExecuteWorkflow(ExternalAgentWorkflow, ExternalAgentInput{
		SessionID: uuid.New(),
		PromptDir: writeTestPrompt(t),
		Config:    testExternalAgentConfig(),
	})

	require.True(t, env.IsWorkflowCompleted())
	require.Error(t, env.GetWorkflowError())

	// Verify state via query handler: the workflow sets status before returning.
	result, err := env.QueryWorkflow(ExternalAgentStateQuery)
	require.NoError(t, err)
	var state externalAgentState
	require.NoError(t, result.Get(&state))
	require.Equal(t, "phase_failed", state.Status)
	require.Equal(t, "main", state.CurrentPhase)
}

func TestShouldRunHook(t *testing.T) {
	tests := []struct {
		when   string
		status string
		want   bool
	}{
		{"", "ok", true},
		{"success", "ok", true},
		{"always", "ok", true},
		{"failure", "ok", false},
		{"", "phase_failed", false},
		{"success", "phase_failed", false},
		{"failure", "phase_failed", true},
		{"always", "phase_failed", true},
		{"failure", "budget_exceeded", true},
		{"always", "budget_exceeded", true},
		{"always", "cancelled", true},
		{"unknown", "ok", false},
	}
	for _, tt := range tests {
		name := fmt.Sprintf("when=%q/status=%s", tt.when, tt.status)
		t.Run(name, func(t *testing.T) {
			hook := agentcfg.StateHook{When: tt.when}
			assert.Equal(t, tt.want, shouldRunHook(hook, tt.status))
		})
	}
}
