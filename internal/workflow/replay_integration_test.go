//go:build integration

package workflow_test

import (
	"encoding/json"
	"log/slog"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
	historypb "go.temporal.io/api/history/v1"
	"go.temporal.io/sdk/log"
	"go.temporal.io/sdk/worker"
	"google.golang.org/protobuf/encoding/protojson"

	wf "github.com/tphakala/alfred/internal/workflow"
)

// replayFixture loads path (a JSON array of protojson-encoded HistoryEvent
// messages, see replay_capture_test.go) and replays it against workflowFn
// via a real worker.WorkflowReplayer -- the same replay mechanism a worker
// uses on every task, and the one that raises NonDeterministicWorkflowError
// on a genuine command-sequence mismatch.
func replayFixture(t *testing.T, path string, workflowFn interface{}) {
	t.Helper()

	f, err := os.Open(path)
	require.NoError(t, err)
	defer func() { require.NoError(t, f.Close()) }()

	var rawEvents []json.RawMessage
	require.NoError(t, json.NewDecoder(f).Decode(&rawEvents))
	require.NotEmpty(t, rawEvents)

	history := &historypb.History{}
	for _, raw := range rawEvents {
		event := &historypb.HistoryEvent{}
		require.NoError(t, protojson.Unmarshal(raw, event))
		history.Events = append(history.Events, event)
	}

	replayer := worker.NewWorkflowReplayer()
	replayer.RegisterWorkflow(workflowFn)
	err = replayer.ReplayWorkflowHistory(log.NewStructuredLogger(slog.Default()), history)
	require.NoError(t, err,
		"the fixed workflow must replay a real pre-#65 marker-less history without NonDeterministicWorkflowError")
}

// TestChatWorkflow_ReplaysPre65MarkerlessHistory replays
// testdata/chat_workflow_pre65_history.json, a REAL history captured from
// the pre-#65 wf.ChatWorkflow (see replay_capture_test.go), which never
// called workflow.GetVersion anywhere and so has NO version markers --
// exactly the shape of any chat session that was already open on this host
// before the six marker checkpoints in this PR landed. It exercises Site A
// (chat-tool-idempotency) and Site F (chat-init-getmaxsequence).
//
// IMPORTANT SCOPE NOTE: because all six markers in this PR are bare,
// non-branching workflow.GetVersion calls, this test does NOT distinguish
// "the #65 fix is present" from "the #65 fix was never applied at all" --
// replaying this exact fixture against a plain revert of this PR (the six
// GetVersion calls simply removed) ALSO passes, since a non-branching
// version-marker call is a no-op on the emitted command sequence by Temporal
// SDK design (confirmed against go.temporal.io/sdk@v1.43.1's own
// TestReplayWorkflowHistory_GetVersion_AddNewBefore test case). What this
// test DOES prove, and is the actual property issue #65's superseded v1
// plan draft would have failed: a design that branches DefaultVersion to a
// RECONSTRUCTED OLD behavior (the naive fix the issue body originally
// suggested) would misroute this exact marker-less-but-current-shape
// history and fail right here with NonDeterministicWorkflowError. That is a
// real, non-vacuous regression guard against ever reintroducing that
// specific, previously-shipped-then-corrected mistake.
func TestChatWorkflow_ReplaysPre65MarkerlessHistory(t *testing.T) {
	replayFixture(t, "testdata/chat_workflow_pre65_history.json", wf.ChatWorkflow)
}

// TestCaseWorkflow_ReplaysPre65MarkerlessHistory is the CaseWorkflow
// counterpart to TestChatWorkflow_ReplaysPre65MarkerlessHistory: it replays
// testdata/case_workflow_pre65_history.json (see
// TestCaptureCaseWorkflowPre65History in replay_capture_test.go) against the
// fixed wf.CaseWorkflow, exercising the two shared internal/workflow/agent_loop.go
// sites -- loop-cost-cap-guard and loop-persist-accepted-terminal -- that
// TestChatWorkflow_ReplaysPre65MarkerlessHistory cannot reach (ChatWorkflow
// has its own separate per-round loop; these two sites are reached only via
// CaseWorkflow and NativeSubAgentWorkflow, both callers of the shared
// runAgentLoop). Same scope note as above applies: this proves safety
// against the superseded branching design, not a red/green distinction
// against a plain revert.
func TestCaseWorkflow_ReplaysPre65MarkerlessHistory(t *testing.T) {
	replayFixture(t, "testdata/case_workflow_pre65_history.json", wf.CaseWorkflow)
}
