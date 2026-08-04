package workflow

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"github.com/tphakala/alfred/internal/agentcfg"
	"github.com/tphakala/alfred/internal/store"
	"go.temporal.io/sdk/testsuite"
)

const (
	candidateKeyFresh = "fresh"
	candidateKeyDone  = "already-done"
	candidateKeyDup   = "dup"
)

func testMonitorConfig(name string) agentcfg.AgentConfig {
	return agentcfg.AgentConfig{
		Name:     name,
		Strategy: agentcfg.StrategyQueueMonitor,
		Prefilter: agentcfg.Prefilter{
			Command: "emit-candidates",
		},
		Monitor: agentcfg.Monitor{
			MaxParallelCases:   5,
			MaxNewCasesPerPass: 20,
			MaxCaseAttempts:    3,
		},
	}
}

func TestQueueMonitorWorkflow_NoCandidatesReturnsCleanly(t *testing.T) {
	ts := &testsuite.WorkflowTestSuite{}
	env := ts.NewTestWorkflowEnvironment()

	a := &MonitorActivities{}
	env.RegisterActivity(a.DedupCandidates)
	env.RegisterActivity(a.CountInProgress)

	env.OnActivity(a.DedupCandidates, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(DedupResult{}, nil)

	pfActivities := &ExternalAgentActivities{}
	env.RegisterActivity(pfActivities.RunPrefilter)
	env.OnActivity(pfActivities.RunPrefilter, mock.Anything, mock.Anything).
		Return(PrefilterResult{}, nil)

	env.ExecuteWorkflow(QueueMonitorWorkflow, QueueMonitorInput{Config: testMonitorConfig("no-candidates")})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	var out QueueMonitorOutcome
	require.NoError(t, env.GetWorkflowResult(&out))
	assert.Equal(t, 0, out.Seen)
	assert.Empty(t, out.Dispatched)
	env.AssertNotCalled(t, "DedupCandidates", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
	env.AssertNotCalled(t, "CountInProgress", mock.Anything, mock.Anything)
}

func TestQueueMonitorWorkflow_DispatchesOnlyDispatchCandidatesSkipsTerminal(t *testing.T) {
	ts := &testsuite.WorkflowTestSuite{}
	env := ts.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(CaseWorkflow)

	monitorActivities := &MonitorActivities{}
	env.RegisterActivity(monitorActivities.DedupCandidates)
	env.RegisterActivity(monitorActivities.CountInProgress)

	pfActivities := &ExternalAgentActivities{}
	env.RegisterActivity(pfActivities.RunPrefilter)
	env.OnActivity(pfActivities.RunPrefilter, mock.Anything, mock.Anything).
		Return(PrefilterResult{Candidates: []PrefilterCandidate{
			{Key: candidateKeyFresh},
			{Key: candidateKeyDone},
		}}, nil)

	env.OnActivity(monitorActivities.DedupCandidates, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(DedupResult{Dispatch: []string{candidateKeyFresh}, Skip: []string{candidateKeyDone}}, nil)
	env.OnActivity(monitorActivities.CountInProgress, mock.Anything, mock.Anything).
		Return(0, nil)

	env.OnWorkflow(CaseWorkflow, mock.Anything, mock.Anything).
		Return(CaseOutcome{Status: store.TaskStatusDone}, nil)

	cfg := testMonitorConfig("dispatch-skip")
	env.ExecuteWorkflow(QueueMonitorWorkflow, QueueMonitorInput{Config: cfg})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	var out QueueMonitorOutcome
	require.NoError(t, env.GetWorkflowResult(&out))
	assert.Equal(t, 2, out.Seen)
	assert.Equal(t, []string{candidateKeyFresh}, out.Dispatched)
	assert.Equal(t, []string{candidateKeyDone}, out.Skipped)

	// The dispatched child must use the deterministic (task, key) workflow ID.
	var childResult CaseOutcome
	require.NoError(t, env.GetWorkflowResultByID(agentcfg.CaseWorkflowID(cfg.Name, candidateKeyFresh), &childResult))
}

func TestQueueMonitorWorkflow_ParallelCapLimitsDispatch(t *testing.T) {
	ts := &testsuite.WorkflowTestSuite{}
	env := ts.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(CaseWorkflow)

	monitorActivities := &MonitorActivities{}
	env.RegisterActivity(monitorActivities.DedupCandidates)
	env.RegisterActivity(monitorActivities.CountInProgress)

	pfActivities := &ExternalAgentActivities{}
	env.RegisterActivity(pfActivities.RunPrefilter)
	env.OnActivity(pfActivities.RunPrefilter, mock.Anything, mock.Anything).
		Return(PrefilterResult{Candidates: []PrefilterCandidate{
			{Key: "a"}, {Key: "b"}, {Key: "c"},
		}}, nil)

	env.OnActivity(monitorActivities.DedupCandidates, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(DedupResult{Dispatch: []string{"a", "b", "c"}}, nil)
	// MaxParallelCases is 5 and 4 cases are already in progress, so only 1
	// slot remains this pass even though 3 candidates are dispatchable and
	// MaxNewCasesPerPass (20) would allow more.
	env.OnActivity(monitorActivities.CountInProgress, mock.Anything, mock.Anything).
		Return(4, nil)

	env.OnWorkflow(CaseWorkflow, mock.Anything, mock.Anything).
		Return(CaseOutcome{Status: store.TaskStatusDone}, nil)

	cfg := testMonitorConfig("cap-parallel")
	env.ExecuteWorkflow(QueueMonitorWorkflow, QueueMonitorInput{Config: cfg})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	var out QueueMonitorOutcome
	require.NoError(t, env.GetWorkflowResult(&out))
	assert.Len(t, out.Dispatched, 1, "only the remaining parallel-cap budget should dispatch this pass")
}

func TestQueueMonitorWorkflow_MaxNewCasesPerPassLimitsDispatch(t *testing.T) {
	ts := &testsuite.WorkflowTestSuite{}
	env := ts.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(CaseWorkflow)

	monitorActivities := &MonitorActivities{}
	env.RegisterActivity(monitorActivities.DedupCandidates)
	env.RegisterActivity(monitorActivities.CountInProgress)

	pfActivities := &ExternalAgentActivities{}
	env.RegisterActivity(pfActivities.RunPrefilter)
	candidates := make([]PrefilterCandidate, 0, 10)
	dispatchKeys := make([]string, 0, 10)
	for i := range 10 {
		key := "k" + string(rune('a'+i))
		candidates = append(candidates, PrefilterCandidate{Key: key})
		dispatchKeys = append(dispatchKeys, key)
	}
	env.OnActivity(pfActivities.RunPrefilter, mock.Anything, mock.Anything).
		Return(PrefilterResult{Candidates: candidates}, nil)

	env.OnActivity(monitorActivities.DedupCandidates, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(DedupResult{Dispatch: dispatchKeys}, nil)
	env.OnActivity(monitorActivities.CountInProgress, mock.Anything, mock.Anything).
		Return(0, nil)

	env.OnWorkflow(CaseWorkflow, mock.Anything, mock.Anything).
		Return(CaseOutcome{Status: store.TaskStatusDone}, nil)

	cfg := testMonitorConfig("cap-per-pass")
	cfg.Monitor.MaxParallelCases = 100
	cfg.Monitor.MaxNewCasesPerPass = 3
	env.ExecuteWorkflow(QueueMonitorWorkflow, QueueMonitorInput{Config: cfg})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	var out QueueMonitorOutcome
	require.NoError(t, env.GetWorkflowResult(&out))
	assert.Len(t, out.Dispatched, 3, "max_new_cases_per_pass must bound dispatch even with ample parallel headroom")
}

func TestQueueMonitorWorkflow_ZeroBudgetDispatchesNothing(t *testing.T) {
	ts := &testsuite.WorkflowTestSuite{}
	env := ts.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(CaseWorkflow)

	monitorActivities := &MonitorActivities{}
	env.RegisterActivity(monitorActivities.DedupCandidates)
	env.RegisterActivity(monitorActivities.CountInProgress)

	pfActivities := &ExternalAgentActivities{}
	env.RegisterActivity(pfActivities.RunPrefilter)
	env.OnActivity(pfActivities.RunPrefilter, mock.Anything, mock.Anything).
		Return(PrefilterResult{Candidates: []PrefilterCandidate{{Key: "a"}}}, nil)

	env.OnActivity(monitorActivities.DedupCandidates, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(DedupResult{Dispatch: []string{"a"}}, nil)
	env.OnActivity(monitorActivities.CountInProgress, mock.Anything, mock.Anything).
		Return(5, nil) // already at MaxParallelCases (5)

	cfg := testMonitorConfig("cap-zero")
	env.ExecuteWorkflow(QueueMonitorWorkflow, QueueMonitorInput{Config: cfg})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	var out QueueMonitorOutcome
	require.NoError(t, env.GetWorkflowResult(&out))
	assert.Empty(t, out.Dispatched)
	env.AssertNotCalled(t, "CaseWorkflow", mock.Anything, mock.Anything)
}

func TestQueueMonitorWorkflow_AlreadyStartedCaseIsSkippedWithoutFailingPass(t *testing.T) {
	ts := &testsuite.WorkflowTestSuite{}
	env := ts.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(CaseWorkflow)

	monitorActivities := &MonitorActivities{}
	env.RegisterActivity(monitorActivities.DedupCandidates)
	env.RegisterActivity(monitorActivities.CountInProgress)
	env.RegisterActivity(monitorActivities.ClaimCase)
	env.RegisterActivity(monitorActivities.FinalizeCase)

	pfActivities := &ExternalAgentActivities{}
	env.RegisterActivity(pfActivities.RunPrefilter)
	// The same candidate key appears twice in one prefilter pass (e.g. the
	// deployment emitter double-reported it). The monitor must not fail the
	// whole pass when the second dispatch collides with the first, still
	// running, case for the same (task, key) workflow ID.
	env.OnActivity(pfActivities.RunPrefilter, mock.Anything, mock.Anything).
		Return(PrefilterResult{Candidates: []PrefilterCandidate{
			{Key: candidateKeyDup}, {Key: candidateKeyDup},
		}}, nil)

	env.OnActivity(monitorActivities.DedupCandidates, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(DedupResult{Dispatch: []string{candidateKeyDup, candidateKeyDup}}, nil)
	env.OnActivity(monitorActivities.CountInProgress, mock.Anything, mock.Anything).
		Return(0, nil)
	env.OnActivity(monitorActivities.ClaimCase, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(monitorActivities.FinalizeCase, mock.Anything, mock.Anything).Return(nil)

	cfg := testMonitorConfig("dup-key")
	env.ExecuteWorkflow(QueueMonitorWorkflow, QueueMonitorInput{Config: cfg})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError(), "an already-started collision must not fail the monitor pass")

	var out QueueMonitorOutcome
	require.NoError(t, env.GetWorkflowResult(&out))
	assert.Len(t, out.Dispatched, 1, "only the first of the two identical-key candidates should be counted dispatched")
}
