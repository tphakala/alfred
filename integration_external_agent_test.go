//go:build integration

package main_test

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"github.com/tphakala/alfred/internal/agent/runner"
	"github.com/tphakala/alfred/internal/agentcfg"
	alfredworkflow "github.com/tphakala/alfred/internal/workflow"
	"go.temporal.io/sdk/testsuite"
)

func TestExternalAgent_MemorySeedRun(t *testing.T) {
	testSuite := &testsuite.WorkflowTestSuite{}
	env := testSuite.NewTestWorkflowEnvironment()

	a := &alfredworkflow.ExternalAgentActivities{}
	env.RegisterActivity(a.RunPreflight)
	env.RegisterActivity(a.RunPrefilter)
	env.RegisterActivity(a.PreparePhaseInput)
	env.RegisterActivity(a.ExecAgentCLI)
	env.RegisterActivity(a.RunCleanup)
	env.RegisterActivity(a.CreateAgentSession)

	env.OnActivity(a.CreateAgentSession, mock.Anything, mock.Anything).Return(nil)

	// Mock prefilter to proceed
	env.OnActivity(a.RunPrefilter, mock.Anything, mock.Anything).
		Return(alfredworkflow.PrefilterResult{
			Proceed: true,
			Data:    map[string]any{"seeded": true},
		}, nil)

	// Mock ExecAgentCLI to return a successful run
	env.OnActivity(a.ExecAgentCLI, mock.Anything, mock.Anything).
		Return(alfredworkflow.ExecOutput{
			ExitCode:     0,
			TotalUsage:   runner.TokenUsage{InputTokens: 100, OutputTokens: 50, TotalTokens: 150},
			TotalCostUSD: 0.005,
			DurationMS:   3000,
			FinalText:    "SEEDED_PRS: 3125,3124\nSEEDED_DISC: 3113\nSKIPPED: 3122",
		}, nil)

	cfg, err := agentcfg.LoadFile("workflows/agents/memory-seed.yaml")
	require.NoError(t, err)

	sessionID := uuid.New()
	env.ExecuteWorkflow(alfredworkflow.ExternalAgentWorkflow, alfredworkflow.ExternalAgentInput{
		SessionID: sessionID,
		Config:    cfg,
		PromptDir: "prompts",
	})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	var outcome alfredworkflow.Outcome
	require.NoError(t, env.GetWorkflowResult(&outcome))
	require.Equal(t, "ok", outcome.Status)

	// Verify prefilter data was stored in phase outputs
	prefilterData, ok := outcome.PhaseOutputs["prefilter"]
	require.True(t, ok, "prefilter data should be in phase outputs")
	prefilterMap, ok := prefilterData.(map[string]any)
	require.True(t, ok)
	require.Equal(t, true, prefilterMap["seeded"])

	// Verify the main phase output
	mainOut, ok := outcome.PhaseOutputs["main"]
	require.True(t, ok, "main phase output should be in phase outputs")
	mainBytes, err := json.Marshal(mainOut)
	require.NoError(t, err)
	var execOut alfredworkflow.ExecOutput
	require.NoError(t, json.Unmarshal(mainBytes, &execOut))
	require.Equal(t, 0, execOut.ExitCode)
	require.Equal(t, 150, execOut.TotalUsage.TotalTokens)
}

func TestExternalAgent_PrefilterNoWork(t *testing.T) {
	testSuite := &testsuite.WorkflowTestSuite{}
	env := testSuite.NewTestWorkflowEnvironment()

	a := &alfredworkflow.ExternalAgentActivities{}
	env.RegisterActivity(a.RunPreflight)
	env.RegisterActivity(a.RunPrefilter)
	env.RegisterActivity(a.PreparePhaseInput)
	env.RegisterActivity(a.ExecAgentCLI)
	env.RegisterActivity(a.RunCleanup)
	env.RegisterActivity(a.CreateAgentSession)

	env.OnActivity(a.CreateAgentSession, mock.Anything, mock.Anything).Return(nil)

	// Mock prefilter to NOT proceed
	env.OnActivity(a.RunPrefilter, mock.Anything, mock.Anything).
		Return(alfredworkflow.PrefilterResult{Proceed: false}, nil)

	cfg, err := agentcfg.LoadFile("workflows/agents/memory-seed.yaml")
	require.NoError(t, err)

	env.ExecuteWorkflow(alfredworkflow.ExternalAgentWorkflow, alfredworkflow.ExternalAgentInput{
		SessionID: uuid.New(),
		Config:    cfg,
		PromptDir: "prompts",
	})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	var outcome alfredworkflow.Outcome
	require.NoError(t, env.GetWorkflowResult(&outcome))
	require.Equal(t, "no_work", outcome.Status)
}
