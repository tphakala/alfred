package workflow

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"github.com/tphakala/alfred/internal/agentcfg"
	"go.temporal.io/sdk/testsuite"
)

func TestExternalAgentWorkflow_ResolveApproval(t *testing.T) {
	ts := &testsuite.WorkflowTestSuite{}
	env := ts.NewTestWorkflowEnvironment()

	a := &ExternalAgentActivities{}
	env.RegisterActivity(a.RunPreflight)
	env.RegisterActivity(a.RunPrefilter)
	env.RegisterActivity(a.PreparePhaseInput)
	env.RegisterActivity(a.ExecAgentCLI)
	env.RegisterActivity(a.RunCleanup)
	env.RegisterActivity(a.CreateAgentSession)

	cfg := agentcfg.AgentConfig{
		Name:     "approval-test",
		Strategy: "external_agent",
		Runner:   "claude",
		Phases: []agentcfg.Phase{{
			Name:     "main",
			Model:    "claude-opus-4-6",
			Prompt:   agentcfg.Prompt{Base: "prompts/agents/x.md"},
			Steering: agentcfg.Steering{Mode: agentcfg.SteeringInteractive},
			Output:   agentcfg.Output{Capture: agentcfg.OutputCaptureText},
		}},
	}

	env.OnActivity(a.CreateAgentSession, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.PreparePhaseInput, mock.Anything, mock.Anything).Return(
		ExecInput{SessionID: uuid.New(), Phase: "main"}, nil,
	)
	env.OnActivity(a.ExecAgentCLI, mock.Anything, mock.Anything).Return(
		ExecOutput{ExitCode: 0}, nil,
	)

	env.ExecuteWorkflow(ExternalAgentWorkflow, ExternalAgentInput{
		SessionID: uuid.New(),
		PromptDir: writeTestPrompt(t),
		Config:    cfg,
	})

	require.True(t, env.IsWorkflowCompleted())
	var outcome Outcome
	require.NoError(t, env.GetWorkflowResult(&outcome))
	require.Equal(t, "ok", outcome.Status)
}

// TestExternalAgentWorkflow_ApprovalHandlersRegistered verifies the approval
// signal and update handlers are registered and the workflow completes
// successfully with interactive steering. The actual approval blocking flow
// (MCP handler -> channel -> resolve) is tested at the HTTP layer in the
// PermitStore and MCP endpoint tests.
func TestExternalAgentWorkflow_ApprovalHandlersRegistered(t *testing.T) {
	ts := &testsuite.WorkflowTestSuite{}
	env := ts.NewTestWorkflowEnvironment()

	sessionID := uuid.New()
	cfg := agentcfg.AgentConfig{
		Name:     "approval-e2e",
		Strategy: "external_agent",
		Runner:   "claude",
		Phases: []agentcfg.Phase{{
			Name:     "main",
			Model:    "claude-opus-4-6",
			Prompt:   agentcfg.Prompt{Base: "prompts/agents/x.md"},
			Steering: agentcfg.Steering{Mode: agentcfg.SteeringInteractive},
			Output:   agentcfg.Output{Capture: agentcfg.OutputCaptureText},
		}},
	}

	a := &ExternalAgentActivities{}
	env.RegisterActivity(a.RunPreflight)
	env.RegisterActivity(a.RunPrefilter)
	env.RegisterActivity(a.PreparePhaseInput)
	env.RegisterActivity(a.ExecAgentCLI)
	env.RegisterActivity(a.RunCleanup)
	env.RegisterActivity(a.CreateAgentSession)

	env.OnActivity(a.CreateAgentSession, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.PreparePhaseInput, mock.Anything, mock.Anything).Return(
		ExecInput{SessionID: sessionID, Phase: "main"}, nil,
	)
	env.OnActivity(a.ExecAgentCLI, mock.Anything, mock.Anything).Return(
		ExecOutput{ExitCode: 0, FinalText: "done"}, nil,
	)

	env.ExecuteWorkflow(ExternalAgentWorkflow, ExternalAgentInput{
		SessionID: sessionID,
		PromptDir: writeTestPrompt(t),
		Config:    cfg,
	})

	require.True(t, env.IsWorkflowCompleted())
	var outcome Outcome
	require.NoError(t, env.GetWorkflowResult(&outcome))
	assert.Equal(t, "ok", outcome.Status)

	// Verify the state query handler works and PendingApprovals map is initialized.
	result, err := env.QueryWorkflow(ExternalAgentStateQuery)
	require.NoError(t, err)
	var state externalAgentState
	require.NoError(t, result.Get(&state))
	assert.NotNil(t, state.PendingApprovals)
}

// TestExternalAgentWorkflow_PendingApprovalsClearedOnResolve verifies the fix
// for issue #19: PendingApprovals must not grow unboundedly. After
// resolve_approval consumes a decision, the entry for that CallID is removed
// from the map so the workflow history payload stays bounded across long runs
// with many tool calls.
func TestExternalAgentWorkflow_PendingApprovalsClearedOnResolve(t *testing.T) {
	ts := &testsuite.WorkflowTestSuite{}
	env := ts.NewTestWorkflowEnvironment()

	sessionID := uuid.New()
	cfg := agentcfg.AgentConfig{
		Name:     "approval-cleanup",
		Strategy: "external_agent",
		Runner:   "claude",
		Phases: []agentcfg.Phase{{
			Name:     "main",
			Model:    "claude-opus-4-6",
			Prompt:   agentcfg.Prompt{Base: "prompts/agents/x.md"},
			Steering: agentcfg.Steering{Mode: agentcfg.SteeringInteractive},
			Output:   agentcfg.Output{Capture: agentcfg.OutputCaptureText},
		}},
	}

	a := &ExternalAgentActivities{}
	env.RegisterActivity(a.RunPreflight)
	env.RegisterActivity(a.RunPrefilter)
	env.RegisterActivity(a.PreparePhaseInput)
	env.RegisterActivity(a.ExecAgentCLI)
	env.RegisterActivity(a.RunCleanup)
	env.RegisterActivity(a.CreateAgentSession)

	env.OnActivity(a.CreateAgentSession, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.PreparePhaseInput, mock.Anything, mock.Anything).Return(
		ExecInput{SessionID: sessionID, Phase: "main"}, nil,
	)
	env.OnActivity(a.ExecAgentCLI, mock.Anything, mock.Anything).Return(
		ExecOutput{ExitCode: 0, FinalText: "done"}, nil,
	)

	// First fire approval_pending, then resolve_approval for the same CallID.
	// After resolution the map must be empty. Both sent in one callback so
	// they reach the workflow within the same simulated tick; the workflow
	// completes too quickly via mocked activities for staggered callbacks to
	// land separately.
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(ExternalAgentApprovalPendingSignal, PermissionRequest{
			RunID:    sessionID.String(),
			CallID:   "call-1",
			ToolName: "Bash",
		})
		env.UpdateWorkflowNoRejection(ExternalAgentResolveApprovalUpdate, "resolve-call-1", t,
			PermissionResolution{CallID: "call-1", Decision: "allow"})
	}, 1*time.Millisecond)

	env.ExecuteWorkflow(ExternalAgentWorkflow, ExternalAgentInput{
		SessionID: sessionID,
		PromptDir: writeTestPrompt(t),
		Config:    cfg,
	})

	require.True(t, env.IsWorkflowCompleted())

	result, err := env.QueryWorkflow(ExternalAgentStateQuery)
	require.NoError(t, err)
	var state externalAgentState
	require.NoError(t, result.Get(&state))
	assert.NotContains(t, state.PendingApprovals, "call-1",
		"resolve_approval must remove the entry; map: %v", state.PendingApprovals)
}
