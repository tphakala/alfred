package workflow

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/tphakala/alfred/internal/agentcfg"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/workflow"
)

// FanoutOutcome collects the results of all child workflows spawned by a
// fanout phase. It is stored in PhaseOutputs under the phase name.
type FanoutOutcome struct {
	Results []Outcome `json:"results"`
}

// fanoutChildRunTimeout bounds each fanout child workflow run. Unchanged from
// the inline literal it replaces.
const fanoutChildRunTimeout = 30 * time.Minute

// buildChildInput constructs the ExternalAgentInput for a single fanout child.
// The child receives a one-phase AgentConfig derived from the parent's fanout
// child config and inherits the parent's runner when the child config does not
// specify one.
func buildChildInput(parent ExternalAgentInput, phase agentcfg.Phase, item any, index int, childSessionID uuid.UUID) ExternalAgentInput { //nolint:gocritic // hugeParam: parent/phase are value copies by design on the fanout path
	fc := phase.Fanout.Child
	childPhase := agentcfg.Phase{
		Name:        fmt.Sprintf("%s-%d", phase.Name, index),
		Model:       fc.Model,
		Prompt:      fc.Prompt,
		Tools:       fc.Tools,
		RunnerFlags: fc.RunnerFlags,
		Steering:    fc.Steering,
		Output:      fc.Output,
	}

	runner := fc.Runner
	if runner == "" {
		runner = parent.Config.Runner
	}

	childConfig := agentcfg.AgentConfig{
		Name:     parent.Config.Name,
		Strategy: parent.Config.Strategy,
		Runner:   runner,
		Phases:   []agentcfg.Phase{childPhase},
		Cleanup:  parent.Config.Cleanup,
		State:    agentcfg.State{Bank: parent.Config.State.Bank},
	}

	return ExternalAgentInput{
		SessionID:       childSessionID,
		ParentSessionID: parent.SessionID,
		Config:          childConfig,
		PromptDir:       parent.PromptDir,
		Item:            item,
		// Inherit the sub-agent identity so a fanout child of a cli sub-agent
		// still rehydrates MCP secrets from the nested sub-agent config. Empty
		// for a top-level task's fanout (resolved by childConfig.Name).
		SubAgentParent: parent.SubAgentParent,
		SubAgentKind:   parent.SubAgentKind,
	}
}

// runFanoutPhase resolves the fanout array, launches child workflows up to the
// configured parallelism limit, and collects their results.
func runFanoutPhase(
	ctx workflow.Context,
	parent ExternalAgentInput, //nolint:gocritic // hugeParam: parent is a value copy by design on the fanout path
	state *externalAgentState,
	phase agentcfg.Phase, //nolint:gocritic // hugeParam: phase is a value copy by design on the fanout path
) (FanoutOutcome, error) {
	items, err := resolvePath(state.PhaseOutputs, phase.Fanout.Over)
	if err != nil {
		return FanoutOutcome{}, fmt.Errorf("fanout %s: %w", phase.Name, err)
	}

	sem := workflow.NewSemaphore(ctx, int64(phase.Fanout.MaxParallel))
	var futures []workflow.ChildWorkflowFuture

	for i, item := range items {
		if err := sem.Acquire(ctx, 1); err != nil {
			return FanoutOutcome{}, fmt.Errorf("fanout %s: semaphore acquire: %w", phase.Name, err)
		}

		var childSessionID uuid.UUID
		encoded := workflow.SideEffect(ctx, func(ctx workflow.Context) any {
			return uuid.New()
		})
		if err := encoded.Get(&childSessionID); err != nil {
			return FanoutOutcome{}, fmt.Errorf("fanout %s: generate child session ID: %w", phase.Name, err)
		}

		childIn := buildChildInput(parent, phase, item, i, childSessionID)
		childIn.Config = agentcfg.StripMCPSecrets(childIn.Config)

		cwfOpts := workflow.WithChildOptions(ctx, workflow.ChildWorkflowOptions{
			WorkflowID:         fmt.Sprintf("%s-%s-%d", state.SessionID, phase.Name, i),
			TaskQueue:          workflow.GetInfo(ctx).TaskQueueName,
			ParentClosePolicy:  enumspb.PARENT_CLOSE_POLICY_TERMINATE,
			WorkflowRunTimeout: fanoutChildRunTimeout,
		})
		f := workflow.ExecuteChildWorkflow(cwfOpts, ExternalAgentWorkflow, childIn)
		futures = append(futures, f)
		workflow.Go(ctx, func(gCtx workflow.Context) {
			_ = f.Get(gCtx, nil)
			sem.Release(1)
		})
	}

	var results []Outcome
	var firstErr error
	for _, f := range futures {
		var r Outcome
		if err := f.Get(ctx, &r); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			results = append(results, Outcome{Status: OutcomeStatusFailed})
			continue
		}
		results = append(results, r)
	}
	return FanoutOutcome{Results: results}, firstErr
}

// resolvePath walks a dot-delimited path through PhaseOutputs and returns the
// array at the final segment. The first segment is a phase name or "prefilter";
// subsequent segments traverse nested maps.
//
// If a segment resolves to an ExecOutput, the function unmarshals its
// StructuredOut JSON and continues the walk from there.
func resolvePath(outputs map[string]any, path string) ([]any, error) {
	segments := strings.Split(path, ".")
	if len(segments) == 0 {
		return nil, fmt.Errorf("resolvePath: empty path")
	}

	var cur any = outputs
	for i, seg := range segments {
		switch v := cur.(type) {
		case map[string]any:
			next, ok := v[seg]
			if !ok {
				return nil, fmt.Errorf("resolvePath: key %q not found at %s", seg, strings.Join(segments[:i+1], "."))
			}
			cur = next
		case ExecOutput:
			if len(v.StructuredOut) == 0 {
				return nil, fmt.Errorf("resolvePath: ExecOutput at %s has no structured output", strings.Join(segments[:i], "."))
			}
			var parsed map[string]any
			if err := json.Unmarshal(v.StructuredOut, &parsed); err != nil {
				return nil, fmt.Errorf("resolvePath: unmarshal StructuredOut at %s: %w", strings.Join(segments[:i], "."), err)
			}
			next, ok := parsed[seg]
			if !ok {
				return nil, fmt.Errorf("resolvePath: key %q not found in StructuredOut at %s", seg, strings.Join(segments[:i+1], "."))
			}
			cur = next
		default:
			return nil, fmt.Errorf("resolvePath: unexpected type %T at %s", cur, strings.Join(segments[:i], "."))
		}
	}

	arr, ok := cur.([]any)
	if !ok {
		return nil, fmt.Errorf("resolvePath: value at %s is %T, not an array", path, cur)
	}
	return arr, nil
}
