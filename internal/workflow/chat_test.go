package workflow_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"

	"github.com/tphakala/alfred/internal/llm"
	wf "github.com/tphakala/alfred/internal/workflow"
)

// Test string constants to avoid goconst lint warnings.
const (
	testKindChat        = "chat"
	testRecallSomething = "recall something"
	testCallIDT1C0      = "t1-c0"
)

func TestChatWorkflow_IdleTimeout(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(wf.ChatWorkflow)
	env.RegisterActivity(&wf.ChatActivities{})

	params := wf.ChatWorkflowParams{
		SessionID: uuid.New(),
		Epoch:     1,
		Kind:      testKindChat,
	}

	var seqCalls int
	env.OnActivity("GetMaxSequence", mock.Anything, mock.Anything).
		Run(func(mock.Arguments) { seqCalls++ }).
		Return(0, nil)

	env.ExecuteWorkflow(wf.ChatWorkflow, params)

	require.True(t, env.IsWorkflowCompleted(), "workflow should complete after idle timeout")
	require.NoError(t, env.GetWorkflowError(), "workflow should complete without error")
	// A session with no turns must run no sequence lookups: the per-turn
	// boundary is fetched by the send/retry handlers, not at workflow init.
	require.Zero(t, seqCalls, "no GetMaxSequence may run before the first turn")
}

func TestChatActivities_Persist_NilStore(t *testing.T) {
	suite := &testsuite.WorkflowTestSuite{}
	env := suite.NewTestActivityEnvironment()

	activities := &wf.ChatActivities{} // nil Store
	env.RegisterActivity(activities)

	sessionID := uuid.New()
	msgs := []wf.ActivityMessage{
		{Role: testRoleUser, Content: testHello, TokenEstimate: 10},
	}

	_, err := env.ExecuteActivity(activities.Persist, sessionID, msgs)
	require.Error(t, err, "Persist with nil store should return an error")
}

// setupBaseMocks registers the activity mocks every per-round agent loop test
// needs, EXCEPT PersistRound (callers choose a plain stub or a capture; a
// plain stub registered first would shadow a capture) and AutoRecall.
func setupBaseMocks(env *testsuite.TestWorkflowEnvironment) {
	env.OnActivity("GetMaxSequence", mock.Anything, mock.Anything).Return(0, nil)
	env.OnActivity("Persist", mock.Anything, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity("ValidatePrompt", mock.Anything, mock.Anything).Return(&wf.ValidatePromptResult{Valid: true}, nil)
	env.OnActivity("GetToolDeclarations", mock.Anything, mock.Anything).Return([]*llm.FunctionDeclaration{}, nil)
	env.OnActivity("BuildContext", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(&wf.BuildContextResult{}, nil)
	env.OnActivity("EmitSSE", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil)
}

// setupMocks registers common activity mocks for the per-round agent loop.
func setupMocks(env *testsuite.TestWorkflowEnvironment) {
	setupBaseMocks(env)
	env.OnActivity("PersistRound", mock.Anything, mock.Anything).Return(nil)
	env.OnActivity("AutoRecall", mock.Anything, mock.Anything).Return(&wf.AutoRecallResult{}, nil)
}

// approvalToolCallRound returns an LLMStream result whose single tool call is
// a request_approval, which the workflow intercepts and awaits on.
func approvalToolCallRound() *wf.LLMStreamResult {
	return &wf.LLMStreamResult{ToolCalls: []wf.LLMToolCall{{
		CallID: testCallIDT1C0,
		Name:   "request_approval",
		Args:   map[string]any{"action": "do_thing", "description": "please approve"},
	}}}
}

// setupApprovalScenario registers the standard mocks for a two-round approval
// turn (round 1 requests an approval, round 2 returns roundTwoText) and a safe
// PersistRound capture. The returned getter must be called only after
// env.ExecuteWorkflow returns; it yields "role: content" lines.
func setupApprovalScenario(env *testsuite.TestWorkflowEnvironment, roundTwoText string) func() []string {
	setupBaseMocks(env)

	// Safe capture shape: ok-guard in the callback (activity goroutine), read
	// only after the workflow completes.
	var persisted []string
	env.OnActivity("PersistRound", mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) {
			if req, ok := args.Get(1).(wf.PersistRoundRequest); ok {
				for _, m := range req.Messages {
					persisted = append(persisted, m.Role+": "+m.Content)
				}
			}
		}).
		Return(nil)

	env.OnActivity("LLMStream", mock.Anything, mock.Anything).Return(approvalToolCallRound(), nil).Once()
	env.OnActivity("LLMStream", mock.Anything, mock.Anything).Return(
		&wf.LLMStreamResult{Text: roundTwoText}, nil,
	).Once()

	return func() []string { return persisted }
}

// persistedLineWith reports whether any captured "role: content" line contains
// all the given substrings.
func persistedLineWith(lines []string, substrs ...string) bool {
	for _, line := range lines {
		ok := true
		for _, s := range substrs {
			if !strings.Contains(line, s) {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

func TestChatWorkflow_ApprovalAwaitSurvivesIdleTimeout(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(wf.ChatWorkflow)
	env.RegisterActivity(&wf.ChatActivities{})

	params := wf.ChatWorkflowParams{
		SessionID: uuid.New(),
		Epoch:     1,
		Kind:      testKindChat,
	}
	getPersisted := setupApprovalScenario(env, "done after approval")

	env.RegisterDelayedCallback(func() {
		env.UpdateWorkflowNoRejection(wf.UpdateSendMessage, "send-1", t,
			wf.SendMessageRequest{Text: "hi"})
	}, time.Millisecond)

	// The human takes longer than DefaultIdleTimeout to decide. The idle timer
	// expires while the turn is blocked on the approval await; it must re-arm
	// instead of completing the workflow out from under the in-progress turn.
	env.RegisterDelayedCallback(func() {
		env.UpdateWorkflowNoRejection(wf.UpdateApproveAction, "approve-1", t,
			wf.ApproveActionRequest{CallID: testCallIDT1C0, Approved: true})
	}, wf.DefaultIdleTimeout+time.Minute)

	env.ExecuteWorkflow(wf.ChatWorkflow, params)

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	persisted := getPersisted()
	require.True(t, persistedLineWith(persisted, "approval_result", `"approved":true`),
		"the late approval must still resolve the await and persist its result, got: %v", persisted)
	require.True(t, persistedLineWith(persisted, "done after approval"),
		"round 2 must run after the late approval (old behavior: workflow completed at the 30 minute mark), got: %v", persisted)
}

func TestChatWorkflow_ApprovalTimesOutAutoRejects(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(wf.ChatWorkflow)
	env.RegisterActivity(&wf.ChatActivities{})

	params := wf.ChatWorkflowParams{
		SessionID: uuid.New(),
		Epoch:     1,
		Kind:      testKindChat,
	}
	getPersisted := setupApprovalScenario(env, "understood, not doing it")

	// The 24h approval wait exceeds the 4h epoch MaxDuration, so the turn's
	// end legitimately triggers an epoch transition; mock its activities.
	env.OnActivity("ExtractEpochSummary", mock.Anything, mock.Anything).
		Return(&wf.ExtractResult{Summary: "epoch summary"}, nil)
	env.OnActivity("RetainToHindsight", mock.Anything, mock.Anything).Return(nil)
	env.OnActivity("PersistEpochTransition", mock.Anything, mock.Anything).Return(nil)
	env.OnActivity("UpdateSessionEpochActivity",
		mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil)

	// Round 1 requests an approval that nobody ever resolves; after
	// DefaultApprovalTimeout it must auto-reject so the turn can finish and
	// the workflow can move on instead of staying open forever.
	env.RegisterDelayedCallback(func() {
		env.UpdateWorkflowNoRejection(wf.UpdateSendMessage, "send-1", t,
			wf.SendMessageRequest{Text: "hi"})
	}, time.Millisecond)

	env.ExecuteWorkflow(wf.ChatWorkflow, params)

	require.True(t, env.IsWorkflowCompleted())
	// The >4h turn deterministically trips the epoch boundary at turn end, so
	// the workflow must exit via ContinueAsNew (couples this test to
	// defaultMaxEpochDuration < DefaultApprovalTimeout, which is the point).
	err := env.GetWorkflowError()
	require.Error(t, err)
	require.True(t, workflow.IsContinueAsNewError(err),
		"the post-timeout turn end must trigger the epoch transition, got: %v", err)

	persisted := getPersisted()
	require.True(t, persistedLineWith(persisted, "approval_result", `"approved":false`, "timed out"),
		"an auto-rejected approval_result with the timeout reason must be persisted, got: %v", persisted)
	require.True(t, persistedLineWith(persisted, "understood, not doing it"),
		"round 2 must run after the auto-rejection, got: %v", persisted)
}

func TestChatWorkflow_RetryAfterFailedBoundaryFetchDoesNotTruncate(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(wf.ChatWorkflow)
	env.RegisterActivity(&wf.ChatActivities{})

	params := wf.ChatWorkflowParams{
		SessionID: uuid.New(),
		Epoch:     2, // a continued session: the DB holds prior-epoch history
		Kind:      testKindChat,
	}

	// The turn-start boundary fetch fails on every attempt (DB outage), so
	// the send turn dies before setting lastPreTurnSequence or persisting
	// anything.
	env.OnActivity("GetMaxSequence", mock.Anything, mock.Anything).
		Return(0, fmt.Errorf("db down"))

	var truncateCalls int
	env.OnActivity("TruncateHistory", mock.Anything, mock.Anything, mock.Anything).
		Run(func(mock.Arguments) { truncateCalls++ }).
		Return(nil)

	env.RegisterDelayedCallback(func() {
		env.UpdateWorkflowNoRejection(wf.UpdateSendMessage, "send-1", t,
			wf.SendMessageRequest{Text: "hi"})
	}, time.Millisecond)

	// The failed send consumed nothing persistent, so a retry has no failed
	// turn to truncate: it must be rejected outright rather than truncating
	// against a boundary the dead turn never set (zero would wipe the whole
	// session's prior-epoch history).
	var rejected error
	env.RegisterDelayedCallback(func() {
		env.UpdateWorkflow(wf.UpdateRetryMessage, "retry-1", &testsuite.TestUpdateCallback{
			OnReject:   func(err error) { rejected = err },
			OnAccept:   func() {},
			OnComplete: func(any, error) {},
		}, wf.RetryMessageRequest{Text: "hi again"})
	}, time.Second)

	env.ExecuteWorkflow(wf.ChatWorkflow, params)

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	require.Error(t, rejected, "retry after a turn that never started must be rejected")
	require.ErrorContains(t, rejected, "cannot retry")
	require.Zero(t, truncateCalls, "no truncation may run against a boundary the failed turn never set")
}

func TestChatWorkflow_TextResponse(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(wf.ChatWorkflow)
	env.RegisterActivity(&wf.ChatActivities{})

	params := wf.ChatWorkflowParams{
		SessionID: uuid.New(),
		Epoch:     1,
		Kind:      testKindChat,
	}

	setupMocks(env)

	// LLM returns text-only response (no tool calls).
	env.OnActivity("LLMStream", mock.Anything, mock.Anything).Return(
		&wf.LLMStreamResult{Text: "Hello from the LLM!"}, nil,
	)

	env.RegisterDelayedCallback(func() {
		env.UpdateWorkflowNoRejection(wf.UpdateSendMessage, "v2-send", t,
			wf.SendMessageRequest{Text: "hi"})
	}, time.Millisecond)

	env.ExecuteWorkflow(wf.ChatWorkflow, params)

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
}

func TestChatWorkflow_ValidationRejection(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(wf.ChatWorkflow)
	env.RegisterActivity(&wf.ChatActivities{})

	params := wf.ChatWorkflowParams{
		SessionID: uuid.New(),
		Epoch:     1,
		Kind:      testKindChat,
	}

	env.OnActivity("GetMaxSequence", mock.Anything, mock.Anything).Return(0, nil)
	env.OnActivity("Persist", mock.Anything, mock.Anything, mock.Anything).Return(nil)

	// ValidatePrompt returns invalid.
	env.OnActivity("ValidatePrompt", mock.Anything, mock.Anything).Return(
		&wf.ValidatePromptResult{Valid: false, Reason: "nonsensical input"}, nil,
	)
	env.OnActivity("PersistAndEmitRejection", mock.Anything, mock.Anything).Return(nil)
	env.OnActivity("EmitSSE", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil)

	env.RegisterDelayedCallback(func() {
		env.UpdateWorkflowNoRejection(wf.UpdateSendMessage, "v2-rejected", t,
			wf.SendMessageRequest{Text: "asdfjkl"})
	}, time.Millisecond)

	env.ExecuteWorkflow(wf.ChatWorkflow, params)

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
}

func TestChatWorkflow_ToolCallRound(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(wf.ChatWorkflow)
	env.RegisterActivity(&wf.ChatActivities{})

	// Register the real dynamic tool activity, matching production wiring
	// (cmd/alfred/main.go): a single handler resolves the tool by whatever
	// activity name the workflow scheduled it under.
	activities := &wf.ChatActivities{}
	env.RegisterDynamicActivity(activities.DynamicToolActivity, activity.DynamicRegisterOptions{})

	params := wf.ChatWorkflowParams{
		SessionID: uuid.New(),
		Epoch:     1,
		Kind:      testKindChat,
	}

	setupMocks(env)

	// Round 1: LLM returns a tool call.
	env.OnActivity("LLMStream", mock.Anything, mock.Anything).Return(
		&wf.LLMStreamResult{
			Text: "Let me look that up.",
			ToolCalls: []wf.LLMToolCall{{
				CallID: testCallIDT1C0,
				Name:   testToolRecall,
				Args:   map[string]any{"query": "test"},
			}},
		}, nil,
	).Once()

	// Mock the dynamic tool activity (dispatched by tool name).
	env.OnActivity(testToolRecall, mock.Anything, mock.Anything).Return(
		&wf.ExecToolResult{CallID: testCallIDT1C0, Name: testToolRecall, Result: `{"memories":[]}`}, nil,
	)

	// Round 2: LLM returns text-only (completes).
	env.OnActivity("LLMStream", mock.Anything, mock.Anything).Return(
		&wf.LLMStreamResult{Text: "I found the answer."}, nil,
	).Once()

	env.RegisterDelayedCallback(func() {
		env.UpdateWorkflowNoRejection(wf.UpdateSendMessage, "v2-tool", t,
			wf.SendMessageRequest{Text: testRecallSomething})
	}, time.Millisecond)

	env.ExecuteWorkflow(wf.ChatWorkflow, params)

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
}

func TestChatWorkflow_RetryMessage(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(wf.ChatWorkflow)
	env.RegisterActivity(&wf.ChatActivities{})

	params := wf.ChatWorkflowParams{
		SessionID: uuid.New(),
		Epoch:     1,
		Kind:      testKindChat,
	}

	setupMocks(env)
	env.OnActivity("TruncateHistory", mock.Anything, mock.Anything, mock.Anything).Return(nil)

	// First send: LLM error.
	env.OnActivity("LLMStream", mock.Anything, mock.Anything).Return(
		(*wf.LLMStreamResult)(nil), fmt.Errorf("LLM unavailable"),
	).Once()

	// Retry: LLM succeeds.
	env.OnActivity("LLMStream", mock.Anything, mock.Anything).Return(
		&wf.LLMStreamResult{Text: "Hello!"}, nil,
	).Once()

	env.RegisterDelayedCallback(func() {
		env.UpdateWorkflowNoRejection(wf.UpdateSendMessage, "v2-send-fail", t,
			wf.SendMessageRequest{Text: testHello})
	}, time.Millisecond)

	env.RegisterDelayedCallback(func() {
		env.UpdateWorkflowNoRejection(wf.UpdateRetryMessage, "v2-retry", t,
			wf.RetryMessageRequest{Text: testHello})
	}, 2*time.Millisecond)

	env.ExecuteWorkflow(wf.ChatWorkflow, params)

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
}

// updateResultCapture captures a workflow update's Reject callback so a test in
// this external test package can assert the update was accepted (the internal
// captureUpdateCallbacks lives in the workflow package's own test file).
type updateResultCapture struct {
	accepted bool
	rejected error
}

func (c *updateResultCapture) Accept()                 { c.accepted = true }
func (c *updateResultCapture) Reject(err error)        { c.rejected = err }
func (c *updateResultCapture) Complete(_ any, _ error) {}

// TestChatWorkflow_EarlyUpdateNotRejected guards the fix for issue #62.
// ChatWorkflow registers its update handlers before the workflow first blocks
// (the main loop's selector; the init-time GetMaxSequence that originally
// motivated the fix was later removed), so a send_message update that arrives
// immediately after start finds a registered handler. If any blocking call is
// ever reintroduced between workflow start and handler registration, the
// 1ms-delayed update lands before any handler is registered and the test env
// rejects it as "unknown update send_message. KnownUpdates=[]". The window is
// timing-sensitive, so this exercises the early-update path across many runs.
func TestChatWorkflow_EarlyUpdateNotRejected(t *testing.T) {
	const iterations = 300
	for i := range iterations {
		var suite testsuite.WorkflowTestSuite
		env := suite.NewTestWorkflowEnvironment()
		env.RegisterWorkflow(wf.ChatWorkflow)
		env.RegisterActivity(&wf.ChatActivities{})

		params := wf.ChatWorkflowParams{SessionID: uuid.New(), Epoch: 1, Kind: testKindChat}
		setupMocks(env)
		env.OnActivity("LLMStream", mock.Anything, mock.Anything).Return(
			&wf.LLMStreamResult{Text: "Hello from the LLM!"}, nil,
		)

		cbs := &updateResultCapture{}
		env.RegisterDelayedCallback(func() {
			env.UpdateWorkflow(wf.UpdateSendMessage, "v2-send", cbs, wf.SendMessageRequest{Text: "hi"})
		}, time.Millisecond)

		env.ExecuteWorkflow(wf.ChatWorkflow, params)
		require.True(t, env.IsWorkflowCompleted())
		require.NoError(t, env.GetWorkflowError())
		require.NoError(t, cbs.rejected,
			"send_message must not be rejected (iter %d): update handlers must be registered before the workflow first blocks", i)
		require.True(t, cbs.accepted,
			"send_message must be accepted and processed (iter %d), not silently buffered and dropped at idle timeout", i)
	}
}
