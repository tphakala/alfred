//go:build capture

package workflow_test

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/tphakala/alfred/internal/agentcfg"
	"github.com/tphakala/alfred/internal/llm"
	"github.com/tphakala/alfred/internal/store"
	wf "github.com/tphakala/alfred/internal/workflow"
)

// This file is deliberately gated behind its own "capture" build tag,
// distinct from "integration": Taskfile's test:integration and test:all
// targets run `go test -tags integration ./...` with no name filter, and
// this file's whole purpose is to record a real Temporal history from the
// PRE-#65 (unguarded) workflow shape. If it shared the "integration" tag, a
// routine `task test:integration` run against a live Temporal server on
// this branch would silently re-execute these captures against the NOW-FIXED
// workflows and overwrite testdata/*.json with marker-bearing histories,
// permanently destroying the "genuinely marker-less" property the replay
// tests in replay_integration_test.go depend on, with no test failure to
// signal it (caught independently by four gate reviewers on this PR). The
// "capture" tag is never referenced by any Taskfile target, so these tests
// only run via an explicit `go test -tags capture ./internal/workflow/...`.
//
// As a second, belt-and-suspenders guard, every capture below also requires
// the ALFRED_REGENERATE_REPLAY_FIXTURES=1 environment variable and skips
// (not fails) otherwise, so even an explicit -tags capture invocation does
// not regenerate a fixture by accident.

const (
	replayServerAddr     = "localhost:7234"
	chatHistoryFixture   = "testdata/chat_workflow_pre65_history.json"
	caseHistoryFixture   = "testdata/case_workflow_pre65_history.json"
	regenerateFixtureEnv = "ALFRED_REGENERATE_REPLAY_FIXTURES"
)

func requireRegenerateOptIn(t *testing.T) {
	t.Helper()
	if os.Getenv(regenerateFixtureEnv) != "1" {
		t.Skipf("skipping fixture regeneration: set %s=1 to intentionally overwrite the checked-in replay fixtures", regenerateFixtureEnv)
	}
}

// writeHistoryFixture fetches sessionID's/runID's full history from c and
// writes it as a JSON array of protojson-encoded events to path. Asserts the
// write succeeded (a truncated fixture would silently defeat the sibling
// replay test with no signal), matching this package's t.Context() /
// context.WithoutCancel(t.Context()) convention for test-scoped I/O.
func writeHistoryFixture(t *testing.T, c client.Client, wfID, runID, path string) int {
	t.Helper()
	iter := c.GetWorkflowHistory(t.Context(), wfID, runID, false, 0)
	var rawEvents []json.RawMessage
	for iter.HasNext() {
		event, err := iter.Next()
		require.NoError(t, err)
		b, err := protojson.Marshal(event)
		require.NoError(t, err)
		rawEvents = append(rawEvents, b)
	}
	require.NotEmpty(t, rawEvents)

	require.NoError(t, os.MkdirAll("testdata", 0o755))
	f, err := os.Create(path)
	require.NoError(t, err)
	require.NoError(t, json.NewEncoder(f).Encode(rawEvents))
	require.NoError(t, f.Close(), "fixture write must flush cleanly; a silent truncation here would defeat the sibling replay test")
	return len(rawEvents)
}

func activityOpts(name string) activity.RegisterOptions {
	return activity.RegisterOptions{Name: name}
}

// --- ChatWorkflow capture (Sites A and F) ---

// The stub* activities below stand in for ChatActivities' real (DB/LLM
// backed) methods, registered under the exact activity type names
// wf.ChatWorkflow schedules by name. They let this capture run against a
// real Temporal server with no database or LLM provider wired up: this test
// only needs the real command SEQUENCE the current (pre-#65) ChatWorkflow
// produces, not real business-logic results.

func stubGetMaxSequence(_ context.Context, _ uuid.UUID) (int, error) { return 0, nil }

func stubPersist(_ context.Context, _ uuid.UUID, _ []wf.ActivityMessage) error { return nil }

func stubValidatePrompt(_ context.Context, _ wf.ValidatePromptRequest) (*wf.ValidatePromptResult, error) {
	return &wf.ValidatePromptResult{Valid: true}, nil
}

func stubGetToolDeclarations(_ context.Context, _ []string) ([]*llm.FunctionDeclaration, error) {
	return nil, nil
}

func stubToolIdempotency(_ context.Context, _ []string) (map[string]bool, error) {
	return map[string]bool{}, nil
}

func stubBuildContext(_ context.Context, _ uuid.UUID, _ int, _ string) (*wf.BuildContextResult, error) {
	return &wf.BuildContextResult{}, nil
}

func stubLLMStream(_ context.Context, _ wf.LLMStreamRequest) (*wf.LLMStreamResult, error) { //nolint:gocritic // hugeParam: activity params are value-semantic on this call path, matching production LLMStream
	return &wf.LLMStreamResult{Text: "captured response, no tool calls"}, nil
}

func stubPersistRound(_ context.Context, _ wf.PersistRoundRequest) error { return nil }

func stubEmitSSE(_ context.Context, _ uuid.UUID, _ string, _ map[string]any) error { return nil }

// TestCaptureChatWorkflowPre65History is a one-shot fixture generator, not a
// normal regression test: it runs the CURRENT (pre-#65-fix) wf.ChatWorkflow
// against the live local Temporal to capture a real marker-less,
// current-shape history (one send_message turn: ValidatePrompt ->
// GetToolDeclarations -> ToolIdempotency [Site A] -> BuildContext -> LLMStream
// (text-only, no tool calls) -> PersistRound -> EmitSSE x2), then saves it as
// a JSON fixture consumed by TestChatWorkflow_ReplaysPre65MarkerlessHistory.
// Also exercises Site F (the ABSENCE of an init-time GetMaxSequence call).
//
// Requires ALFRED_REGENERATE_REPLAY_FIXTURES=1 and the "capture" build tag
// (see the file-level doc comment); re-run only to intentionally regenerate
// the fixture.
func TestCaptureChatWorkflowPre65History(t *testing.T) {
	requireRegenerateOptIn(t)

	c, err := client.Dial(client.Options{HostPort: replayServerAddr})
	require.NoError(t, err)
	defer c.Close()

	const taskQueue = "test-replay-capture-65-chat"
	w := worker.New(c, taskQueue, worker.Options{})
	w.RegisterWorkflow(wf.ChatWorkflow)
	w.RegisterActivityWithOptions(stubGetMaxSequence, activityOpts("GetMaxSequence"))
	w.RegisterActivityWithOptions(stubPersist, activityOpts("Persist"))
	w.RegisterActivityWithOptions(stubValidatePrompt, activityOpts("ValidatePrompt"))
	w.RegisterActivityWithOptions(stubGetToolDeclarations, activityOpts("GetToolDeclarations"))
	w.RegisterActivityWithOptions(stubToolIdempotency, activityOpts("ToolIdempotency"))
	w.RegisterActivityWithOptions(stubBuildContext, activityOpts("BuildContext"))
	w.RegisterActivityWithOptions(stubLLMStream, activityOpts("LLMStream"))
	w.RegisterActivityWithOptions(stubPersistRound, activityOpts("PersistRound"))
	w.RegisterActivityWithOptions(stubEmitSSE, activityOpts("EmitSSE"))
	require.NoError(t, w.Start())
	defer w.Stop()

	wfID := "test-replay-capture-65-chat-" + uuid.NewString()
	sessionID := uuid.New()
	startCtx, startCancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer startCancel()

	run, err := c.ExecuteWorkflow(startCtx, client.StartWorkflowOptions{
		ID: wfID, TaskQueue: taskQueue,
	}, wf.ChatWorkflow, wf.ChatWorkflowParams{SessionID: sessionID, Epoch: 1, Kind: store.SessionKindChat})
	require.NoError(t, err)
	defer func() {
		// t.Context() is already canceled by the time deferred cleanups run.
		_ = c.TerminateWorkflow(context.WithoutCancel(t.Context()), wfID, run.GetRunID(), "replay capture cleanup") //nolint:errcheck // best-effort cleanup of a throwaway capture workflow
	}()

	updateCtx, updateCancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer updateCancel()
	handle, err := c.UpdateWorkflow(updateCtx, client.UpdateWorkflowOptions{
		WorkflowID:   wfID,
		RunID:        run.GetRunID(),
		UpdateName:   wf.UpdateSendMessage,
		Args:         []interface{}{wf.SendMessageRequest{Text: "hi"}},
		WaitForStage: client.WorkflowUpdateStageCompleted,
	})
	require.NoError(t, err)
	var resp wf.SendMessageResponse
	require.NoError(t, handle.Get(updateCtx, &resp))
	require.Empty(t, resp.Error, "the captured turn must complete cleanly")

	n := writeHistoryFixture(t, c, wfID, run.GetRunID(), chatHistoryFixture)
	t.Logf("captured %d history events to %s", n, chatHistoryFixture)
}

// --- CaseWorkflow capture (Sites B and C) ---

func stubClaimCase(_ context.Context, _ wf.ClaimCaseArgs) error { return nil } //nolint:gocritic // hugeParam: activity params are value-semantic on this call path, matching production ClaimCase

func stubFinalizeCase(_ context.Context, _ wf.FinalizeCaseArgs) error { return nil }

// stubCaseLLMStream returns a record_outcome tool call on every round, so the
// captured run accepts a terminal call on round 1: this exercises Site C
// (persistAcceptedTerminal) in addition to Site B (the cost-cap-guard
// checkpoint, which runs unconditionally once per runAgentLoop call before
// the round loop even starts).
func stubCaseLLMStream(_ context.Context, _ wf.CaseLLMStreamRequest) (*wf.CaseLLMStreamResult, error) { //nolint:gocritic // hugeParam: activity params are value-semantic on this call path, matching production CaseLLMStream
	return &wf.CaseLLMStreamResult{
		ToolCalls: []wf.LLMToolCall{{
			CallID: "capture-c1",
			Name:   agentcfg.ReservedToolRecordOutcome,
			Args:   map[string]any{"status": store.TaskStatusDone},
		}},
	}, nil
}

// TestCaptureCaseWorkflowPre65History is a one-shot fixture generator,
// companion to TestCaptureChatWorkflowPre65History: it runs the CURRENT
// (pre-#65-fix) wf.CaseWorkflow to capture a real marker-less history
// exercising the two agent_loop.go sites (loop-cost-cap-guard,
// loop-persist-accepted-terminal), shared by both CaseWorkflow and
// NativeSubAgentWorkflow, which the chat fixture above cannot reach.
//
// Requires ALFRED_REGENERATE_REPLAY_FIXTURES=1 and the "capture" build tag
// (see the file-level doc comment); re-run only to intentionally regenerate
// the fixture.
func TestCaptureCaseWorkflowPre65History(t *testing.T) {
	requireRegenerateOptIn(t)

	c, err := client.Dial(client.Options{HostPort: replayServerAddr})
	require.NoError(t, err)
	defer c.Close()

	const taskQueue = "test-replay-capture-65-case"
	w := worker.New(c, taskQueue, worker.Options{})
	w.RegisterWorkflow(wf.CaseWorkflow)
	w.RegisterActivityWithOptions(stubClaimCase, activityOpts("ClaimCase"))
	w.RegisterActivityWithOptions(stubFinalizeCase, activityOpts("FinalizeCase"))
	w.RegisterActivityWithOptions(stubBuildContext, activityOpts("BuildContext"))
	w.RegisterActivityWithOptions(stubPersistRound, activityOpts("PersistRound"))
	w.RegisterActivityWithOptions(stubCaseLLMStream, activityOpts("CaseLLMStream"))
	require.NoError(t, w.Start())
	defer w.Stop()

	wfID := "test-replay-capture-65-case-" + uuid.NewString()
	startCtx, startCancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer startCancel()

	in := wf.CaseInput{
		Task:         "capture-task",
		CandidateKey: "capture-key",
		Context:      json.RawMessage(`{"title":"capture"}`),
		Config: agentcfg.AgentConfig{
			Name:     "capture-task",
			Strategy: agentcfg.StrategyQueueMonitor,
		},
	}
	run, err := c.ExecuteWorkflow(startCtx, client.StartWorkflowOptions{
		ID: wfID, TaskQueue: taskQueue,
	}, wf.CaseWorkflow, in)
	require.NoError(t, err)
	defer func() {
		_ = c.TerminateWorkflow(context.WithoutCancel(t.Context()), wfID, run.GetRunID(), "replay capture cleanup") //nolint:errcheck // best-effort cleanup of a throwaway capture workflow
	}()

	var out wf.CaseOutcome
	require.NoError(t, run.Get(startCtx, &out), "the captured case must complete cleanly")
	require.Equal(t, store.TaskStatusDone, out.Status)

	n := writeHistoryFixture(t, c, wfID, run.GetRunID(), caseHistoryFixture)
	t.Logf("captured %d history events to %s", n, caseHistoryFixture)
}
