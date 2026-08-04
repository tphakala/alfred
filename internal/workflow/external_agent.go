package workflow

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/tphakala/alfred/internal/agent/runner"
	"github.com/tphakala/alfred/internal/agentcfg"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

// Handler names exported by ExternalAgentWorkflow. Cross-package callers
// (REST handlers, in-process updaters, tests) must reference these constants
// rather than the literal strings so the wire contract has a single source
// of truth.
const (
	ExternalAgentStateQuery            = "state"
	ExternalAgentCancelUpdate          = "cancel_run"
	ExternalAgentResolveApprovalUpdate = "resolve_approval"
	ExternalAgentInjectGuidanceUpdate  = "inject_guidance"
	ExternalAgentApprovalPendingSignal = "approval_pending"
)

// ExternalAgentInput is the entry shape for ExternalAgentWorkflow.
// ParentSessionID is set when this workflow is launched as a fanout child;
// it is recorded on the child session row so the parent-child relationship
// is reconstructable from the sessions table.
// Item is set when this workflow is launched as a fanout child; it carries the
// per-iteration element from the parent's fanout array and is exposed to the
// child's prompt templates as the .item variable.
type ExternalAgentInput struct {
	SessionID       uuid.UUID
	ParentSessionID uuid.UUID
	Config          agentcfg.AgentConfig
	PromptDir       string
	Item            any
	// SubAgentParent and SubAgentKind mark this workflow as a Case Supervisor
	// cli sub-agent: SubAgentParent is the parent task's config name and
	// SubAgentKind is its kind under that config's Supervisor.SubAgents. Both
	// empty for a top-level external_agent task. They are threaded to
	// ExecAgentCLI so the sub-agent's stripped MCP secrets can be rehydrated
	// from the worker's unstripped config (its own Config.Name is empty).
	SubAgentParent string
	SubAgentKind   string
}

// Outcome is the workflow result.
type Outcome struct {
	Status       string         `json:"status"`
	FailedAt     string         `json:"failed_at,omitempty"`
	PhaseOutputs map[string]any `json:"phase_outputs,omitempty"`
	CancelReason string         `json:"cancel_reason,omitempty"`
	TotalCostUSD float64        `json:"total_cost_usd,omitempty"`
}

// Outcome.Status values shared by every sub-agent backend and the fanout
// path. The engine treats these as opaque; they exist so the producers
// cannot drift apart (a typo becomes a compile error).
const (
	OutcomeStatusOK                  = "ok"
	OutcomeStatusFailed              = "failed"
	OutcomeStatusBudgetExceeded      = "budget_exceeded"
	OutcomeStatusSessionCreateFailed = "session_create_failed"
	OutcomeStatusCancelled           = "cancelled"
	OutcomeStatusNoWork              = "no_work"
	OutcomeStatusPhaseFailed         = "phase_failed"
	OutcomeStatusPreflightFailed     = "preflight_failed"
	OutcomeStatusPrefilterFailed     = "prefilter_failed"
)

// Activity-option timeouts and retry tuning for ExternalAgentWorkflow. Named
// to match their call sites; values are unchanged from the inline literals
// they replace. Retry-attempt counts reuse the package-wide
// defaultRetryMaxAttempts, and the prefilter timeout reuses
// prefilterActivityTimeout.
const (
	cleanupActivityTimeout   = 2 * time.Minute  // best-effort cleanup steps
	sessionActivityTimeout   = 30 * time.Second // CreateAgentSession row insert
	prefilterRetryBackoff    = 4.0              // RunPrefilter retry backoff
	onCompleteHookTimeout    = 2 * time.Minute  // OnComplete state hooks
	execActivityStartToClose = 30 * time.Minute // ExecAgentCLI upper bound
	execHeartbeatTimeout     = 2 * time.Minute  // ExecAgentCLI heartbeat window
)

type externalAgentState struct {
	SessionID        uuid.UUID
	Status           string
	CurrentPhase     string
	PhaseOutputs     map[string]any
	CancelReason     string
	PendingApprovals map[string]PermissionResolution
	GuidanceLog      []GuidanceLogEntry
}

// ExternalAgentWorkflow orchestrates an external_agent run: preflight checks,
// prefilter gate, sequential phase execution, and best-effort cleanup.
func ExternalAgentWorkflow(ctx workflow.Context, in ExternalAgentInput) (Outcome, error) { //nolint:gocognit,gocyclo,gocritic // gocognit/gocyclo: inherently-complex workflow orchestrator (preflight, prefilter, phase loop, cleanup); hugeParam: workflow inputs are value-semantic for Temporal serialization
	state := &externalAgentState{
		SessionID:        in.SessionID,
		Status:           "running",
		PhaseOutputs:     map[string]any{},
		PendingApprovals: map[string]PermissionResolution{},
	}

	if err := workflow.SetQueryHandler(ctx, ExternalAgentStateQuery, func() (*externalAgentState, error) {
		return state, nil
	}); err != nil {
		return Outcome{}, err
	}

	phaseCtx, cancelPhases := workflow.WithCancel(ctx)
	if err := workflow.SetUpdateHandler(ctx, ExternalAgentCancelUpdate, func(_ workflow.Context, reason string) error {
		state.Status = OutcomeStatusCancelled
		state.CancelReason = reason
		cancelPhases()
		return nil
	}); err != nil {
		return Outcome{}, err
	}

	// Approval state tracking for Tier 2 per-call steering. The actual
	// approval gate is the PermitStore's in-process channel (not Temporal).
	// The PendingApprovals map exists purely for query visibility: it tracks
	// which call IDs are awaiting a decision. resolve_approval removes the
	// entry so the map stays bounded across long-running interactive runs;
	// the resolved decision itself is broadcast through the PermitStore
	// channel for the activity to consume.
	if err := workflow.SetUpdateHandler(ctx, ExternalAgentResolveApprovalUpdate, func(_ workflow.Context, res PermissionResolution) error {
		delete(state.PendingApprovals, res.CallID)
		return nil
	}); err != nil {
		return Outcome{}, err
	}

	if err := workflow.SetUpdateHandlerWithOptions(ctx, ExternalAgentInjectGuidanceUpdate,
		func(uCtx workflow.Context, req GuidanceRequest) (GuidanceLogEntry, error) {
			if !req.RunnerCapsBidirectionalStream {
				return GuidanceLogEntry{}, &GuidanceUnsupportedError{Runner: in.Config.Runner}
			}
			entry := GuidanceLogEntry{
				Text:       req.Text,
				SequenceNo: len(state.GuidanceLog) + 1,
				ReceivedAt: workflow.Now(uCtx),
			}
			state.GuidanceLog = append(state.GuidanceLog, entry)
			return entry, nil
		},
		workflow.UpdateHandlerOptions{
			Validator: func(_ workflow.Context, req GuidanceRequest) error {
				if req.Text == "" {
					return fmt.Errorf("guidance text is empty")
				}
				return nil
			},
		},
	); err != nil {
		return Outcome{}, err
	}

	// Records pending approval requests in workflow state for the "state" query.
	approvalCh := workflow.GetSignalChannel(ctx, ExternalAgentApprovalPendingSignal)
	workflow.Go(ctx, func(gCtx workflow.Context) {
		for {
			var req PermissionRequest
			if !approvalCh.Receive(gCtx, &req) {
				return
			}
			state.PendingApprovals[req.CallID] = PermissionResolution{}
		}
	})

	// Best-effort cleanup runs even after cancellation, so it uses a
	// disconnected context for both the activity options and the Get() call.
	defer func() {
		disconnected, cleanupCancel := workflow.NewDisconnectedContext(ctx)
		defer cleanupCancel()
		opts := workflow.WithActivityOptions(disconnected, workflow.ActivityOptions{
			StartToCloseTimeout: cleanupActivityTimeout,
			RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 1},
		})
		for _, step := range in.Config.Cleanup {
			_ = workflow.ExecuteActivity(opts, "RunCleanup", step).Get(disconnected, nil)
		}
	}()

	// Generate a session ID if the caller did not provide one (e.g. cron-triggered runs).
	if in.SessionID == uuid.Nil {
		encoded := workflow.SideEffect(ctx, func(ctx workflow.Context) any {
			return uuid.New()
		})
		var generated uuid.UUID
		if err := encoded.Get(&generated); err != nil {
			return Outcome{}, err
		}
		in.SessionID = generated
		state.SessionID = in.SessionID
	}

	// Create the sessions row so the FK on messages.session_id is satisfied.
	sessionOpts := workflow.WithActivityOptions(phaseCtx, workflow.ActivityOptions{
		StartToCloseTimeout: sessionActivityTimeout,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: defaultRetryMaxAttempts},
	})
	wfInfo := workflow.GetInfo(ctx)
	var fanoutItem json.RawMessage
	if in.Item != nil {
		b, err := json.Marshal(in.Item)
		if err != nil {
			return Outcome{}, fmt.Errorf("marshal fanout item: %w", err)
		}
		fanoutItem = b
	}
	if err := workflow.ExecuteActivity(sessionOpts, "CreateAgentSession",
		CreateAgentSessionArgs{
			SessionID:       in.SessionID,
			WorkflowID:      wfInfo.WorkflowExecution.ID,
			RunID:           wfInfo.WorkflowExecution.RunID,
			ParentSessionID: in.ParentSessionID,
			FanoutItem:      fanoutItem,
		},
	).Get(phaseCtx, nil); err != nil {
		state.Status = OutcomeStatusSessionCreateFailed
		return Outcome{Status: state.Status}, err
	}

	// Preflight: sequential gate checks, fail fast on any error.
	preflightOpts := workflow.WithActivityOptions(phaseCtx, workflow.ActivityOptions{
		StartToCloseTimeout: 1 * time.Minute,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 1},
	})
	for _, step := range in.Config.Preflight {
		if err := workflow.ExecuteActivity(preflightOpts, "RunPreflight", step).Get(phaseCtx, nil); err != nil {
			state.Status = OutcomeStatusPreflightFailed
			return Outcome{Status: state.Status, FailedAt: step.Name}, err
		}
	}

	// Prefilter: determine whether there is work to do.
	var pf PrefilterResult
	if in.Config.Prefilter.Type != "" || in.Config.Prefilter.Command != "" {
		prefilterOpts := workflow.WithActivityOptions(phaseCtx, workflow.ActivityOptions{
			StartToCloseTimeout: prefilterActivityTimeout,
			RetryPolicy: &temporal.RetryPolicy{
				InitialInterval:    1 * time.Second,
				BackoffCoefficient: prefilterRetryBackoff,
				MaximumAttempts:    defaultRetryMaxAttempts,
			},
		})
		if err := workflow.ExecuteActivity(prefilterOpts, "RunPrefilter", in.Config.Prefilter).Get(phaseCtx, &pf); err != nil {
			state.Status = OutcomeStatusPrefilterFailed
			return Outcome{Status: state.Status}, err
		}
		if !pf.Proceed {
			state.Status = OutcomeStatusNoWork
			return Outcome{Status: state.Status}, nil
		}
		state.PhaseOutputs["prefilter"] = pf.Data
	}

	// OnComplete hooks run after the phase loop (even on failure) but before
	// cleanup. Defers run LIFO: this defer is registered AFTER the cleanup defer
	// (above), so it executes BEFORE cleanup. Do not reorder these two defers.
	defer func() {
		if len(in.Config.State.OnComplete) == 0 {
			return
		}
		disconnected, hookCancel := workflow.NewDisconnectedContext(ctx)
		defer hookCancel()
		hookOpts := workflow.WithActivityOptions(disconnected, workflow.ActivityOptions{
			StartToCloseTimeout: onCompleteHookTimeout,
			RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 1},
		})
		for _, hook := range in.Config.State.OnComplete {
			if shouldRunHook(hook, state.Status) {
				_ = workflow.ExecuteActivity(hookOpts, "RunStateHook", hook).Get(disconnected, nil)
			}
		}
	}()

	// Execute phases sequentially. Per-host pinning via a dedicated task queue
	// is configured at the schedule/trigger level, not hardcoded here.
	execOpts := workflow.WithActivityOptions(phaseCtx, workflow.ActivityOptions{
		StartToCloseTimeout: execActivityStartToClose,
		HeartbeatTimeout:    execHeartbeatTimeout,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 1},
	})
	budgetCap := in.Config.Budgets.WorkflowMaxCostUSD
	var cumulativeCost float64

	for _, phase := range in.Config.Phases { //nolint:gocritic // rangeValCopy: agentcfg.Phase is value-semantic on the phase-loop path
		state.CurrentPhase = phase.Name

		if phase.Fanout != nil {
			fanoutOut, ferr := runFanoutPhase(phaseCtx, in, state, phase)
			if ferr != nil {
				if state.Status == OutcomeStatusCancelled {
					return Outcome{Status: state.Status, FailedAt: phase.Name, CancelReason: state.CancelReason, TotalCostUSD: cumulativeCost}, nil
				}
				state.Status = OutcomeStatusPhaseFailed
				return Outcome{Status: state.Status, FailedAt: phase.Name, TotalCostUSD: cumulativeCost}, ferr
			}
			for _, r := range fanoutOut.Results {
				cumulativeCost += r.TotalCostUSD
			}
			state.PhaseOutputs[phase.Name] = fanoutOut
		} else {
			var execIn ExecInput
			if err := workflow.ExecuteActivity(sessionOpts, "PreparePhaseInput", PreparePhaseInputArgs{
				SessionID:      in.SessionID,
				Phase:          phase,
				ConfigName:     in.Config.Name,
				Runner:         in.Config.Runner,
				PhaseOutputs:   state.PhaseOutputs,
				PromptDir:      in.PromptDir,
				Item:           in.Item,
				StateBank:      in.Config.State.Bank,
				SubAgentParent: in.SubAgentParent,
				SubAgentKind:   in.SubAgentKind,
			}).Get(phaseCtx, &execIn); err != nil {
				if state.Status == OutcomeStatusCancelled {
					return Outcome{Status: state.Status, FailedAt: phase.Name, CancelReason: state.CancelReason, TotalCostUSD: cumulativeCost}, nil
				}
				state.Status = OutcomeStatusPhaseFailed
				return Outcome{Status: state.Status, FailedAt: phase.Name, TotalCostUSD: cumulativeCost}, err
			}
			var out ExecOutput
			if err := workflow.ExecuteActivity(execOpts, "ExecAgentCLI", execIn).Get(phaseCtx, &out); err != nil {
				if state.Status == OutcomeStatusCancelled {
					return Outcome{Status: state.Status, FailedAt: phase.Name, CancelReason: state.CancelReason, TotalCostUSD: cumulativeCost}, nil
				}
				state.Status = OutcomeStatusPhaseFailed
				return Outcome{Status: state.Status, FailedAt: phase.Name, TotalCostUSD: cumulativeCost}, err
			}
			cumulativeCost += out.TotalCostUSD
			state.PhaseOutputs[phase.Name] = out
		}

		// Budget check runs after both fanout and non-fanout paths.
		// Enforcement is post-hoc: the just-completed phase's cost is already
		// spent, but remaining phases are skipped. Per-phase limits are handled
		// by the runner's --max-budget-usd flag.
		if budgetCap > 0 && cumulativeCost > budgetCap {
			state.Status = OutcomeStatusBudgetExceeded
			state.CurrentPhase = ""
			return Outcome{Status: state.Status, FailedAt: phase.Name, PhaseOutputs: state.PhaseOutputs, TotalCostUSD: cumulativeCost}, nil
		}
	}
	state.CurrentPhase = ""

	state.Status = OutcomeStatusOK
	return Outcome{Status: state.Status, PhaseOutputs: state.PhaseOutputs, TotalCostUSD: cumulativeCost}, nil
}

// shouldRunHook decides whether an OnComplete hook should execute based on
// its When condition and the current workflow status.
func shouldRunHook(hook agentcfg.StateHook, status string) bool {
	switch hook.When {
	case "always":
		return true
	case "success", "":
		return status == OutcomeStatusOK
	case "failure":
		return status != OutcomeStatusOK
	default:
		return false
	}
}

// runnerRunConfigFromPhase maps agentcfg types to the runner-agnostic RunConfig.
func runnerRunConfigFromPhase(phase agentcfg.Phase, prompt string) runner.RunConfig { //nolint:gocritic // hugeParam: agentcfg.Phase is value-semantic on the config mapping path
	return runner.RunConfig{
		Model:                phase.Model,
		Prompt:               prompt,
		MaxTurns:             phase.RunnerFlags.MaxTurns,
		MaxBudgetUSD:         phase.RunnerFlags.MaxBudgetUSD,
		Bare:                 derefBoolOr(phase.RunnerFlags.Bare, true),
		StrictMCPConfig:      derefBoolOr(phase.RunnerFlags.StrictMCPConfig, true),
		NoSessionPersistence: derefBoolOr(phase.RunnerFlags.NoSessionPersistence, true),
		Steering:             runner.SteeringMode(phase.Steering.Mode),
		Tools: runner.ToolPolicy{
			Bash:    phase.Tools.Bash,
			Builtin: phase.Tools.Builtin,
		},
		MCP:           runner.MCPPolicy{Servers: convertMCPServers(phase.Tools.MCP.Servers)},
		OutputCapture: runner.OutputCaptureMode(phase.Output.Capture),
	}
}

func derefBoolOr(p *bool, def bool) bool {
	if p == nil {
		return def
	}
	return *p
}

func convertMCPServers(src map[string]agentcfg.MCPServerConfig) map[string]runner.MCPServerConfig {
	if len(src) == 0 {
		return nil
	}
	out := make(map[string]runner.MCPServerConfig, len(src))
	for name, cfg := range src {
		out[name] = runner.MCPServerConfig{
			Type:    cfg.Type,
			Command: cfg.Command,
			Args:    cfg.Args,
			URL:     cfg.URL,
			Headers: cfg.Headers,
			Env:     cfg.Env,
		}
	}
	return out
}
