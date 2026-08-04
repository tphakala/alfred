package workflow

import (
	"encoding/json"
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

func TestResolvePath_SimpleKey(t *testing.T) {
	outputs := map[string]any{
		"scout": map[string]any{
			"subtasks": []any{
				map[string]any{"issue_number": 1},
				map[string]any{"issue_number": 2},
			},
		},
	}
	items, err := resolvePath(outputs, "scout.subtasks")
	require.NoError(t, err)
	require.Len(t, items, 2)
	assert.Equal(t, 1, items[0].(map[string]any)["issue_number"])
}

func TestResolvePath_PrefilterKey(t *testing.T) {
	outputs := map[string]any{
		"prefilter": map[string]any{
			"candidates": []any{10, 20, 30},
		},
	}
	items, err := resolvePath(outputs, "prefilter.candidates")
	require.NoError(t, err)
	require.Len(t, items, 3)
}

func TestResolvePath_MissingKey(t *testing.T) {
	outputs := map[string]any{}
	_, err := resolvePath(outputs, "scout.subtasks")
	assert.ErrorContains(t, err, "scout")
}

func TestResolvePath_NotAnArray(t *testing.T) {
	outputs := map[string]any{
		"scout": map[string]any{
			"subtasks": "not an array",
		},
	}
	_, err := resolvePath(outputs, "scout.subtasks")
	assert.ErrorContains(t, err, "not an array")
}

func TestResolvePath_DeepPath(t *testing.T) {
	outputs := map[string]any{
		"analysis": map[string]any{
			"results": map[string]any{
				"items": []any{"a", "b"},
			},
		},
	}
	items, err := resolvePath(outputs, "analysis.results.items")
	require.NoError(t, err)
	assert.Len(t, items, 2)
}

func TestResolvePath_ExecOutputJSON(t *testing.T) {
	outputs := map[string]any{
		"scout": ExecOutput{
			StructuredOut: []byte(`{"subtasks":[{"id":1},{"id":2}]}`),
		},
	}
	items, err := resolvePath(outputs, "scout.subtasks")
	require.NoError(t, err)
	require.Len(t, items, 2)
}

func TestBuildChildInput_SetsFields(t *testing.T) {
	parentSessionID := uuid.New()
	parent := ExternalAgentInput{
		SessionID: parentSessionID,
		Config: agentcfg.AgentConfig{
			Name:   "parent",
			Runner: "claude",
		},
		PromptDir: "/prompts",
	}
	phase := agentcfg.Phase{
		Name: "per_issue",
		Fanout: &agentcfg.FanoutConfig{
			Over:        "scout.subtasks",
			MaxParallel: 4,
			Child: agentcfg.FanoutChildConfig{
				Model:       "claude-opus-4-6",
				Prompt:      agentcfg.Prompt{Base: "child.md"},
				Tools:       agentcfg.Tools{Bash: []string{"git*"}},
				RunnerFlags: agentcfg.RunnerFlags{MaxTurns: 40},
				Steering:    agentcfg.Steering{Mode: agentcfg.SteeringScheduled},
				Output:      agentcfg.Output{Capture: agentcfg.OutputCaptureText},
			},
		},
	}
	item := map[string]any{"issue_number": float64(42)}
	childSessionID := uuid.New()

	childIn := buildChildInput(parent, phase, item, 3, childSessionID)

	assert.Equal(t, childSessionID, childIn.SessionID)
	assert.Equal(t, parentSessionID, childIn.ParentSessionID,
		"child must record the parent's session ID so the FK can be set when the child session row is created")
	assert.Equal(t, "parent", childIn.Config.Name)
	assert.Equal(t, "claude", childIn.Config.Runner)
	require.Len(t, childIn.Config.Phases, 1)
	assert.Equal(t, "per_issue-3", childIn.Config.Phases[0].Name)
	assert.Equal(t, "claude-opus-4-6", childIn.Config.Phases[0].Model)
	assert.Equal(t, "/prompts", childIn.PromptDir)
	assert.Equal(t, item, childIn.Item,
		"child must record the per-iteration item so PreparePhaseInput can expose it as the .item template variable")
}

func TestBuildChildInput_InheritsStateBank(t *testing.T) {
	parent := ExternalAgentInput{
		SessionID: uuid.New(),
		Config: agentcfg.AgentConfig{
			Name:   "parent",
			Runner: "claude",
			State:  agentcfg.State{Bank: "alfred-tickets"},
		},
	}
	phase := agentcfg.Phase{
		Name: "per_issue",
		Fanout: &agentcfg.FanoutConfig{
			Over:        "scout.subtasks",
			MaxParallel: 2,
			Child: agentcfg.FanoutChildConfig{
				Model:    "claude-opus-4-6",
				Prompt:   agentcfg.Prompt{Base: "child.md"},
				Steering: agentcfg.Steering{Mode: agentcfg.SteeringScheduled},
				Output:   agentcfg.Output{Capture: agentcfg.OutputCaptureText},
			},
		},
	}
	childIn := buildChildInput(parent, phase, nil, 0, uuid.New())
	assert.Equal(t, "alfred-tickets", childIn.Config.State.Bank,
		"fanout children must inherit parent's State.Bank for HINDSIGHT_BANK env injection")
	assert.Empty(t, childIn.Config.State.OnComplete,
		"fanout children must not inherit OnComplete hooks (parent runs those once)")
}

func TestExternalAgentWorkflow_Fanout_DispatchesChildren(t *testing.T) {
	var s testsuite.WorkflowTestSuite
	env := s.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(ExternalAgentWorkflow)

	sessionID := uuid.New()
	cfg := newFanoutScoutPerIssueConfig("fanout-test")

	// Register activities so the test environment can resolve them by name
	// for both parent and child workflows.
	a := &ExternalAgentActivities{}
	env.RegisterActivity(a.RunPreflight)
	env.RegisterActivity(a.RunPrefilter)
	env.RegisterActivity(a.PreparePhaseInput)
	env.RegisterActivity(a.ExecAgentCLI)
	env.RegisterActivity(a.RunCleanup)
	env.RegisterActivity(a.CreateAgentSession)

	// Activities are called for both parent and child workflows. Capture
	// every CreateAgentSession invocation so we can assert that children
	// reference the parent via ParentSessionID.
	var sessionMu sync.Mutex
	var sessionCalls []CreateAgentSessionArgs
	env.OnActivity(a.CreateAgentSession, mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) {
			sessionMu.Lock()
			defer sessionMu.Unlock()
			if argVal, ok := args.Get(1).(CreateAgentSessionArgs); ok {
				sessionCalls = append(sessionCalls, argVal)
			}
		}).Return(nil)
	env.OnActivity(a.PreparePhaseInput, mock.Anything, mock.Anything).Return(
		ExecInput{
			SessionID:  sessionID,
			Runner:     "claude",
			Phase:      "scout",
			ConfigName: "fanout-test",
			Config:     runner.RunConfig{Model: "claude-haiku-4-5", Prompt: "scout prompt"},
		}, nil,
	)
	env.OnActivity(a.ExecAgentCLI, mock.Anything, mock.Anything).Return(
		ExecOutput{
			ExitCode:      0,
			StructuredOut: json.RawMessage(`{"subtasks":[{"issue_number":1},{"issue_number":2}]}`),
		}, nil,
	)

	env.ExecuteWorkflow(ExternalAgentWorkflow, ExternalAgentInput{
		SessionID: sessionID,
		Config:    cfg,
		PromptDir: ".",
	})

	require.True(t, env.IsWorkflowCompleted())
	err := env.GetWorkflowError()
	require.NoError(t, err, "workflow should complete without error")

	var outcome Outcome
	require.NoError(t, env.GetWorkflowResult(&outcome))
	assert.Equal(t, "ok", outcome.Status)

	// CreateAgentSession was called for the parent and for each fanout child.
	// The parent must have no ParentSessionID; each child must point at the
	// parent's session ID.
	sessionMu.Lock()
	defer sessionMu.Unlock()
	require.GreaterOrEqual(t, len(sessionCalls), 3, "parent + 2 children expected")

	var parentCalls, childCalls int
	for _, call := range sessionCalls {
		if call.SessionID == sessionID {
			parentCalls++
			assert.Equal(t, uuid.Nil, call.ParentSessionID,
				"parent session must not have a ParentSessionID, got %s", call.ParentSessionID)
		} else {
			childCalls++
			assert.Equal(t, sessionID, call.ParentSessionID,
				"child session %s must point to parent %s, got %s",
				call.SessionID, sessionID, call.ParentSessionID)
		}
	}
	assert.Equal(t, 1, parentCalls, "exactly one parent CreateAgentSession call expected")
	assert.GreaterOrEqual(t, childCalls, 2, "two fanout children expected")
}

func TestExternalAgentWorkflow_Fanout_PassesItemToChildPrepare(t *testing.T) {
	var s testsuite.WorkflowTestSuite
	env := s.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(ExternalAgentWorkflow)

	sessionID := uuid.New()
	cfg := newFanoutScoutPerIssueConfig("fanout-item-test")

	a := &ExternalAgentActivities{}
	env.RegisterActivity(a.RunPreflight)
	env.RegisterActivity(a.RunPrefilter)
	env.RegisterActivity(a.PreparePhaseInput)
	env.RegisterActivity(a.ExecAgentCLI)
	env.RegisterActivity(a.RunCleanup)
	env.RegisterActivity(a.CreateAgentSession)

	env.OnActivity(a.CreateAgentSession, mock.Anything, mock.Anything).Return(nil)

	// Capture every PreparePhaseInput invocation so the test can prove that
	// each fanout child received the correct per-iteration item under
	// PreparePhaseInputArgs.Item.
	var prepMu sync.Mutex
	var prepCalls []PreparePhaseInputArgs
	env.OnActivity(a.PreparePhaseInput, mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) {
			prepMu.Lock()
			defer prepMu.Unlock()
			if argVal, ok := args.Get(1).(PreparePhaseInputArgs); ok {
				prepCalls = append(prepCalls, argVal)
			}
		}).Return(
		ExecInput{
			SessionID:  sessionID,
			Runner:     "claude",
			Phase:      "scout",
			ConfigName: "fanout-item-test",
			Config:     runner.RunConfig{Model: "claude-haiku-4-5", Prompt: "scout prompt"},
		}, nil,
	)
	env.OnActivity(a.ExecAgentCLI, mock.Anything, mock.Anything).Return(
		ExecOutput{
			ExitCode:      0,
			StructuredOut: json.RawMessage(`{"subtasks":[{"issue_number":1},{"issue_number":2}]}`),
		}, nil,
	)

	env.ExecuteWorkflow(ExternalAgentWorkflow, ExternalAgentInput{
		SessionID: sessionID,
		Config:    cfg,
		PromptDir: ".",
	})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	prepMu.Lock()
	defer prepMu.Unlock()

	var scoutCall *PreparePhaseInputArgs
	childCalls := map[string]PreparePhaseInputArgs{}
	for i := range prepCalls {
		c := prepCalls[i]
		switch c.Phase.Name {
		case "scout":
			scoutCall = &c
		case "per_issue-0", "per_issue-1":
			childCalls[c.Phase.Name] = c
		}
	}

	require.NotNil(t, scoutCall, "scout PreparePhaseInput call expected")
	assert.Nil(t, scoutCall.Item,
		"non-fanout (parent) phases must not have an Item set; .item is fanout-only")

	require.Contains(t, childCalls, "per_issue-0")
	require.Contains(t, childCalls, "per_issue-1")
	// resolvePath unmarshals StructuredOut via encoding/json so numbers become float64.
	assert.Equal(t,
		map[string]any{"issue_number": float64(1)},
		childCalls["per_issue-0"].Item,
		"fanout child 0 must receive the first subtask as its Item")
	assert.Equal(t,
		map[string]any{"issue_number": float64(2)},
		childCalls["per_issue-1"].Item,
		"fanout child 1 must receive the second subtask as its Item")
}

// newFanoutScoutPerIssueConfig builds the standard two-phase
// (scout + per_issue fanout) AgentConfig used by the fanout workflow tests.
func newFanoutScoutPerIssueConfig(name string) agentcfg.AgentConfig {
	return agentcfg.AgentConfig{
		Name:     name,
		Strategy: "external_agent",
		Runner:   "claude",
		Phases: []agentcfg.Phase{
			{
				Name:     "scout",
				Model:    "claude-haiku-4-5",
				Prompt:   agentcfg.Prompt{Base: "scout.md"},
				Steering: agentcfg.Steering{Mode: agentcfg.SteeringScheduled},
				Output:   agentcfg.Output{Capture: agentcfg.OutputCaptureStructured},
			},
			{
				Name:     "per_issue",
				Model:    "claude-opus-4-6",
				Prompt:   agentcfg.Prompt{Base: "unused.md"},
				Steering: agentcfg.Steering{Mode: agentcfg.SteeringScheduled},
				Output:   agentcfg.Output{Capture: agentcfg.OutputCaptureText},
				Fanout: &agentcfg.FanoutConfig{
					Over:        "scout.subtasks",
					MaxParallel: 2,
					Child: agentcfg.FanoutChildConfig{
						Model:    "claude-opus-4-6",
						Prompt:   agentcfg.Prompt{Base: "child.md"},
						Steering: agentcfg.Steering{Mode: agentcfg.SteeringScheduled},
						Output:   agentcfg.Output{Capture: agentcfg.OutputCaptureText},
					},
				},
			},
		},
	}
}
