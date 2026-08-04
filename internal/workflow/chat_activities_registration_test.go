package workflow_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"

	"github.com/tphakala/alfred/internal/agent"
	wf "github.com/tphakala/alfred/internal/workflow"
)

// TestChatActivities_DynamicToolRegistration pins the real Temporal worker
// registration sequence cmd/alfred/main.go uses for ChatWorkflow's tool
// dispatch. It uses a lazy (non-dialing) client so it stays a fast unit test
// while still exercising the SAME registry validation the production
// worker.Worker performs. testsuite.TestWorkflowEnvironment does not
// reproduce this class of duplicate-activity-registration bug (its mock
// registry is more permissive), which is why it never caught #116: the
// worker panicked at startup with any LLM provider configured because
// DynamicToolActivity was registered twice under the same derived name,
// once via the blanket struct scan and once via a RegisterActivityWithOptions
// call with an empty Name (which is NOT Temporal's dynamic-activity API).
func TestChatActivities_DynamicToolRegistration(t *testing.T) {
	c, err := client.NewLazyClient(client.Options{})
	require.NoError(t, err)
	defer c.Close()

	w := worker.New(c, "test-queue", worker.Options{})
	activities := &wf.ChatActivities{}

	assert.NotPanics(t, func() {
		w.RegisterActivity(activities)
		w.RegisterDynamicActivity(activities.DynamicToolActivity, activity.DynamicRegisterOptions{})
	})
}

// dynamicDispatchTestWorkflow schedules an activity by a runtime-chosen name
// that is never individually registered, forcing Temporal to fall through to
// the dynamic activity handler. This is what ChatWorkflow's real tool
// dispatch does (workflow.ExecuteActivity(ctx, call.Name, toolReq)).
func dynamicDispatchTestWorkflow(ctx workflow.Context, toolName string, req wf.ExecToolRequest) (*wf.ExecToolResult, error) {
	ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{StartToCloseTimeout: time.Minute})
	var result wf.ExecToolResult
	if err := workflow.ExecuteActivity(ctx, toolName, req).Get(ctx, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// TestChatActivities_DynamicToolActivity_DecodesArgsAndDispatches proves the
// fixed DynamicToolActivity signature actually decodes converter.EncodedValues
// back into ExecToolRequest and dispatches to the resolved tool name, not just
// that registration no longer panics (#116).
func TestChatActivities_DynamicToolActivity_DecodesArgsAndDispatches(t *testing.T) {
	registry := agent.NewRegistry(&fakeIdempotencyTool{name: testToolRecall, idempotent: true})
	activities := &wf.ChatActivities{Registry: registry}

	suite := &testsuite.WorkflowTestSuite{}
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(dynamicDispatchTestWorkflow)
	env.RegisterActivity(activities)
	env.RegisterDynamicActivity(activities.DynamicToolActivity, activity.DynamicRegisterOptions{})

	req := wf.ExecToolRequest{
		SessionID: uuid.New(),
		TurnID:    1,
		CallID:    "call-1",
		Args:      map[string]any{"query": "test"},
	}
	env.ExecuteWorkflow(dynamicDispatchTestWorkflow, testToolRecall, req)

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	var result wf.ExecToolResult
	require.NoError(t, env.GetWorkflowResult(&result))
	assert.Equal(t, "call-1", result.CallID)
	assert.Equal(t, testToolRecall, result.Name)
	assert.Empty(t, result.Error)
	assert.Equal(t, "{}", result.Result, "fakeIdempotencyTool.Execute always returns an empty object")
}

// TestChatActivities_DynamicToolActivity_UnknownToolReturnsError proves the
// error-formatting branch of DynamicToolActivity (adjacent to the #116 fix)
// through the same real dynamic-dispatch path: an empty registry means
// agent.SafeExecute returns agent.ErrToolNotFound, which must surface as a
// non-nil ExecToolResult.Error, not as an activity/workflow failure.
func TestChatActivities_DynamicToolActivity_UnknownToolReturnsError(t *testing.T) {
	activities := &wf.ChatActivities{Registry: agent.NewRegistry()}

	suite := &testsuite.WorkflowTestSuite{}
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(dynamicDispatchTestWorkflow)
	env.RegisterActivity(activities)
	env.RegisterDynamicActivity(activities.DynamicToolActivity, activity.DynamicRegisterOptions{})

	const unknownTool = "no_such_tool"
	req := wf.ExecToolRequest{SessionID: uuid.New(), TurnID: 1, CallID: "call-2"}
	env.ExecuteWorkflow(dynamicDispatchTestWorkflow, unknownTool, req)

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	var result wf.ExecToolResult
	require.NoError(t, env.GetWorkflowResult(&result))
	assert.Equal(t, "call-2", result.CallID)
	assert.Equal(t, unknownTool, result.Name)
	assert.Contains(t, result.Error, "tool not found")
	assert.Contains(t, result.Result, "error:")
}

// TestChatActivities_DynamicToolRegistration_ReservedNameStillResolves proves
// the blanket w.RegisterActivity(chatActivities) struct scan does register
// DynamicToolActivity's own Go method name as a second, ordinary (reachable)
// activity entry alongside the dynamic registration -- the exact hazard the
// #116 gate review converged on. ChatWorkflow's tool-dispatch guard
// (dynamicToolActivityMethodName in chat.go) is what actually prevents a
// tool call from ever reaching it in production; this test documents WHY
// that guard exists by showing the reserved name is live at the registry
// level, not merely a theoretical name collision.
func TestChatActivities_DynamicToolRegistration_ReservedNameStillResolves(t *testing.T) {
	activities := &wf.ChatActivities{Registry: agent.NewRegistry()}

	suite := &testsuite.WorkflowTestSuite{}
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(dynamicDispatchTestWorkflow)
	env.RegisterActivity(activities)
	env.RegisterDynamicActivity(activities.DynamicToolActivity, activity.DynamicRegisterOptions{})

	req := wf.ExecToolRequest{SessionID: uuid.New(), TurnID: 1, CallID: "call-3"}
	env.ExecuteWorkflow(dynamicDispatchTestWorkflow, "DynamicToolActivity", req)

	require.True(t, env.IsWorkflowCompleted())
	// Resolves to the blanket-scan's ordinary (non-dynamic) registration,
	// which tries to bind converter.EncodedValues by reflection and fails
	// decoding -- an activity/workflow-level error, not a clean dispatch.
	require.Error(t, env.GetWorkflowError())
}
