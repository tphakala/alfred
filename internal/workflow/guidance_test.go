package workflow

import (
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"github.com/tphakala/alfred/internal/agentcfg"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
)

// captureUpdateCallbacks captures the result of an update invocation so tests
// can assert on the outcome without blocking.
type captureUpdateCallbacks struct {
	accepted bool
	rejected error
	result   any
	err      error
}

func (c *captureUpdateCallbacks) Accept()                        { c.accepted = true }
func (c *captureUpdateCallbacks) Reject(err error)               { c.rejected = err }
func (c *captureUpdateCallbacks) Complete(result any, err error) { c.result = result; c.err = err }

// guidanceTestCfg returns a minimal config for guidance tests. It uses no
// prefilter so the workflow reaches ExecAgentCLI in one fewer step, making
// the 1ms delayed callback fire during the ExecAgentCLI activity (the last
// blocking point before workflow completion).
func guidanceTestCfg(runner string) agentcfg.AgentConfig {
	return agentcfg.AgentConfig{
		Name:   "test",
		Runner: runner,
		Phases: []agentcfg.Phase{{
			Name:     "main",
			Steering: agentcfg.Steering{Mode: agentcfg.SteeringScheduled},
		}},
	}
}

// registerGuidanceActivities registers all activities needed by
// ExternalAgentWorkflow (no prefilter variant) with standard mocks.
// ExecAgentCLI blocks for holdDur workflow-clock time so that a 1ms delayed
// callback has a chance to fire mid-run.
func registerGuidanceActivities(env *testsuite.TestWorkflowEnvironment, holdDur time.Duration) {
	a := &ExternalAgentActivities{}
	env.RegisterActivity(a.CreateAgentSession)
	env.RegisterActivity(a.RunPrefilter)
	env.RegisterActivity(a.PreparePhaseInput)
	env.RegisterActivity(a.ExecAgentCLI)
	env.RegisterActivity(a.RunCleanup)

	env.OnActivity(a.CreateAgentSession, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.PreparePhaseInput, mock.Anything, mock.Anything).Return(ExecInput{}, nil)
	// ExecAgentCLI is scheduled with a 30-minute StartToCloseTimeout (see
	// external_agent.go). The delayed callback fires after holdDur of workflow
	// clock has elapsed, which the mock clock advances during this activity.
	env.OnActivity(a.ExecAgentCLI, mock.Anything, mock.Anything).
		After(holdDur).
		Return(ExecOutput{ExitCode: 0}, nil)
}

func TestInjectGuidance_RecordsLogEntry(t *testing.T) {
	ts := &testsuite.WorkflowTestSuite{}
	env := ts.NewTestWorkflowEnvironment()

	registerGuidanceActivities(env, 10*time.Millisecond)

	cbs := &captureUpdateCallbacks{}
	// Fire the update at 1ms; ExecAgentCLI holds for 10ms so the callback
	// fires mid-activity and reaches the registered handler.
	env.RegisterDelayedCallback(func() {
		env.UpdateWorkflow(ExternalAgentInjectGuidanceUpdate, "guid-1", cbs, GuidanceRequest{
			RunnerCapsBidirectionalStream: true,
			Text:                          "look at #42",
		})
	}, 1*time.Millisecond)

	env.ExecuteWorkflow(ExternalAgentWorkflow, ExternalAgentInput{
		Config: guidanceTestCfg("claude"),
	})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	assert.True(t, cbs.accepted, "update should be accepted")
	assert.NoError(t, cbs.rejected, "update should not be rejected")
	require.NoError(t, cbs.err)
	entry, ok := cbs.result.(GuidanceLogEntry)
	require.True(t, ok, "result should be GuidanceLogEntry, got %T", cbs.result)
	assert.Equal(t, "look at #42", entry.Text)
	assert.Equal(t, 1, entry.SequenceNo)
}

func TestInjectGuidance_RejectsNonBidiRunner(t *testing.T) {
	ts := &testsuite.WorkflowTestSuite{}
	env := ts.NewTestWorkflowEnvironment()

	registerGuidanceActivities(env, 10*time.Millisecond)

	cbs := &captureUpdateCallbacks{}
	env.RegisterDelayedCallback(func() {
		env.UpdateWorkflow(ExternalAgentInjectGuidanceUpdate, "guid-2", cbs, GuidanceRequest{
			RunnerCapsBidirectionalStream: false,
			Text:                          "this should fail at handler",
		})
	}, 1*time.Millisecond)

	env.ExecuteWorkflow(ExternalAgentWorkflow, ExternalAgentInput{
		Config: guidanceTestCfg("gemini"),
	})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	assert.True(t, cbs.accepted, "update should be accepted (validation passes; non-empty text)")
	require.Error(t, cbs.err, "handler should return GuidanceUnsupportedError")
	typed, ok := errors.AsType[*GuidanceUnsupportedError](cbs.err)
	require.True(t, ok, "error should be GuidanceUnsupportedError, got %T: %v", cbs.err, cbs.err)
	assert.Equal(t, "gemini", typed.Runner)
}

func TestInjectGuidance_RejectsEmptyText(t *testing.T) {
	ts := &testsuite.WorkflowTestSuite{}
	env := ts.NewTestWorkflowEnvironment()

	registerGuidanceActivities(env, 10*time.Millisecond)

	cbs := &captureUpdateCallbacks{}
	env.RegisterDelayedCallback(func() {
		env.UpdateWorkflow(ExternalAgentInjectGuidanceUpdate, "guid-3", cbs, GuidanceRequest{
			RunnerCapsBidirectionalStream: true,
			Text:                          "",
		})
	}, 1*time.Millisecond)

	env.ExecuteWorkflow(ExternalAgentWorkflow, ExternalAgentInput{
		Config: guidanceTestCfg("claude"),
	})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	require.Error(t, cbs.rejected, "validator should reject empty text")
	assert.Contains(t, cbs.rejected.Error(), "guidance text is empty")
}

func TestGuidanceUnsupportedErrorTypeName_MatchesReflectedName(t *testing.T) {
	// Sanity check: the const used to match Temporal-wrapped ApplicationError
	// Type tags must equal the reflection-derived Go type name the SDK uses
	// at the wire boundary. If someone renames GuidanceUnsupportedError they
	// must also update the const; this assertion fails loudly when they do not.
	reflected := reflect.TypeOf(GuidanceUnsupportedError{}).Name()
	assert.Equal(t, guidanceUnsupportedErrorTypeName, reflected,
		"const must match reflect.TypeOf(GuidanceUnsupportedError{}).Name(); update the const after renames")
}

func TestIsGuidanceUnsupportedError(t *testing.T) {
	t.Run("typed error", func(t *testing.T) {
		assert.True(t, IsGuidanceUnsupportedError(&GuidanceUnsupportedError{Runner: "gemini"}))
	})

	t.Run("wrapped typed error", func(t *testing.T) {
		err := fmt.Errorf("dispatch: %w", &GuidanceUnsupportedError{Runner: "copilot"})
		assert.True(t, IsGuidanceUnsupportedError(err))
	})

	t.Run("temporal ApplicationError with matching type", func(t *testing.T) {
		// Simulates the wire form after Temporal serializes the typed error
		// across worker boundaries: the Go type info is lost but the Type tag
		// "GuidanceUnsupportedError" identifies it.
		appErr := temporal.NewNonRetryableApplicationError(
			"runner \"gemini\" does not support mid-run guidance",
			"GuidanceUnsupportedError",
			nil,
		)
		assert.True(t, IsGuidanceUnsupportedError(appErr))
	})

	t.Run("unrelated error", func(t *testing.T) {
		assert.False(t, IsGuidanceUnsupportedError(errors.New("network down")))
		assert.False(t, IsGuidanceUnsupportedError(nil))
	})

	t.Run("ApplicationError with different type", func(t *testing.T) {
		appErr := temporal.NewNonRetryableApplicationError(
			"some other failure", "SomeOtherError", nil,
		)
		assert.False(t, IsGuidanceUnsupportedError(appErr))
	})
}
