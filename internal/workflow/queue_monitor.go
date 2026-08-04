package workflow

import (
	"time"

	"github.com/tphakala/alfred/internal/agentcfg"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

// QueueMonitorInput is the input to QueueMonitorWorkflow.
type QueueMonitorInput struct {
	Config    agentcfg.AgentConfig
	PromptDir string
}

// QueueMonitorOutcome summarizes one monitor pass for observability and tests.
type QueueMonitorOutcome struct {
	Seen       int      `json:"seen"`
	Dispatched []string `json:"dispatched,omitempty"`
	Skipped    []string `json:"skipped,omitempty"`
	Held       []string `json:"held,omitempty"`
}

const (
	prefilterActivityTimeout    = 10 * time.Minute
	prefilterBackoffCoefficient = 4.0
	monitorActivityMaxAttempts  = 3
	ledgerActivityTimeout       = 1 * time.Minute
)

// QueueMonitorWorkflow is the Level 0 queue monitor: a deterministic,
// one-pass dispatcher. It never calls an LLM. A Temporal Schedule re-fires
// it each cadence, so it does not use ContinueAsNew; each invocation runs
// the task's prefilter, dedups candidates against the task_state ledger,
// and starts a Case child workflow for each dispatchable candidate within
// the pass's concurrency budget.
func QueueMonitorWorkflow(ctx workflow.Context, in QueueMonitorInput) (QueueMonitorOutcome, error) { //nolint:gocritic // hugeParam: workflow inputs are value-semantic for Temporal serialization
	prefilterOpts := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: prefilterActivityTimeout,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    time.Second,
			BackoffCoefficient: prefilterBackoffCoefficient,
			MaximumAttempts:    monitorActivityMaxAttempts,
		},
	})
	var pf PrefilterResult
	if err := workflow.ExecuteActivity(prefilterOpts, "RunPrefilter", in.Config.Prefilter).Get(ctx, &pf); err != nil {
		return QueueMonitorOutcome{}, err
	}
	if len(pf.Candidates) == 0 {
		return QueueMonitorOutcome{}, nil
	}

	keys := make([]string, len(pf.Candidates))
	byKey := make(map[string]PrefilterCandidate, len(pf.Candidates))
	for i, c := range pf.Candidates {
		keys[i] = c.Key
		byKey[c.Key] = c
	}

	shortOpts := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: ledgerActivityTimeout,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: monitorActivityMaxAttempts},
	})

	var dedup DedupResult
	if err := workflow.ExecuteActivity(shortOpts, "DedupCandidates",
		in.Config.Name, keys, in.Config.Monitor.MaxCaseAttempts).Get(ctx, &dedup); err != nil {
		return QueueMonitorOutcome{}, err
	}

	outcome := QueueMonitorOutcome{
		Seen:    len(pf.Candidates),
		Skipped: dedup.Skip,
		Held:    dedup.Held,
	}

	if len(dedup.Dispatch) == 0 {
		return outcome, nil
	}

	var inProgress int
	if err := workflow.ExecuteActivity(shortOpts, "CountInProgress", in.Config.Name).Get(ctx, &inProgress); err != nil {
		return outcome, err
	}

	budget := in.Config.Monitor.MaxNewCasesPerPass
	if remaining := in.Config.Monitor.MaxParallelCases - inProgress; remaining < budget {
		budget = remaining
	}
	if budget <= 0 {
		return outcome, nil
	}

	for _, key := range dedup.Dispatch {
		if len(outcome.Dispatched) >= budget {
			break
		}
		started, err := dispatchCase(ctx, in, byKey[key])
		if err != nil {
			if temporal.IsWorkflowExecutionAlreadyStartedError(err) {
				// A case for this candidate is already running; the
				// duplicate dispatch is expected under (task, key)
				// collisions (e.g. a candidate reported twice in one
				// prefilter pass) and must not fail the whole pass.
				continue
			}
			return outcome, err
		}
		if started {
			outcome.Dispatched = append(outcome.Dispatched, key)
		}
	}

	return outcome, nil
}

// dispatchCase starts a Case child workflow for one candidate, fire and
// forget: it confirms the child started (or catches an already-started
// collision) without awaiting the case's own result, so the case outlives
// this monitor pass.
func dispatchCase(ctx workflow.Context, in QueueMonitorInput, cand PrefilterCandidate) (bool, error) { //nolint:gocritic // hugeParam: workflow inputs are value-semantic for Temporal serialization
	childCtx := workflow.WithChildOptions(ctx, workflow.ChildWorkflowOptions{
		WorkflowID:            agentcfg.CaseWorkflowID(in.Config.Name, cand.Key),
		TaskQueue:             workflow.GetInfo(ctx).TaskQueueName,
		WorkflowIDReusePolicy: enumspb.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE,
		ParentClosePolicy:     enumspb.PARENT_CLOSE_POLICY_ABANDON,
	})
	future := workflow.ExecuteChildWorkflow(childCtx, CaseWorkflow, CaseInput{
		Task:         in.Config.Name,
		CandidateKey: cand.Key,
		Context:      cand.Context,
		Config:       in.Config,
		PromptDir:    in.PromptDir,
	})

	var exec workflow.Execution
	if err := future.GetChildWorkflowExecution().Get(ctx, &exec); err != nil {
		return false, err
	}
	return true, nil
}
