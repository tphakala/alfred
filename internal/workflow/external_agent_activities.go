package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"os/exec"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/tphakala/alfred/internal/agent/runner"
	"github.com/tphakala/alfred/internal/agentcfg"
	"github.com/tphakala/alfred/internal/ctxbuild"
	"github.com/tphakala/alfred/internal/store"
)

// dialAddress converts a Go listen address (e.g. ":8086", "0.0.0.0:8086") into
// a loopback-connectable address. server.address in config.yaml is a listen
// address (bind-all-interfaces forms like ":8086" are the documented default),
// but the MCP permission-bridge callback URL built from it is a dial target
// for a same-host subprocess; a bind wildcard host has no meaning there (#118:
// a bare ":8086" produced "http://:8086/...", which every HTTP client rejects
// outright with "no host part in the URL"). Falls back to the input unchanged
// if it isn't a valid host:port (e.g. already just a hostname).
func dialAddress(listenAddr string) string {
	host, port, err := net.SplitHostPort(listenAddr)
	if err != nil {
		return listenAddr
	}
	switch host {
	case "", "0.0.0.0":
		host = "127.0.0.1"
	case "::":
		host = "::1" // IPv6 wildcard needs the IPv6 loopback, not an IPv4 address
	}
	return net.JoinHostPort(host, port)
}

// Interactive-run token TTL, event-channel buffer depth, and heartbeat cadence
// for ExecAgentCLI. Values are unchanged from the inline literals they replace.
const (
	runRegistrationTTL = 30 * time.Minute
	eventChannelBuffer = 64
	heartbeatInterval  = 30 * time.Second
)

// PermitRegistrar is the subset of server.PermitStore needed by ExecAgentCLI
// to register/deregister per-run tokens for the MCP permission bridge.
type PermitRegistrar interface {
	RegisterRun(runID string, ttl time.Duration) string
	DeregisterRun(runID string)
}

// GuidanceRegistrar is the subset of server.GuidanceStore used by
// ExecAgentCLI to register a guidance channel per run and to wire the
// channel into the runner's RunConfig.
type GuidanceRegistrar interface {
	RegisterRun(runID string)
	DeregisterRun(runID string)
	Receiver(runID string) (<-chan string, bool)
}

// EventSink persists normalized Events and broadcasts to UI subscribers.
type EventSink interface {
	Persist(ctx context.Context, sessionID uuid.UUID, phase string, ev runner.Event) error
	Broadcast(sessionID uuid.UUID, ev runner.Event)
}

// PersistedEvent is the test-visible record shape.
type PersistedEvent struct {
	SessionID uuid.UUID
	Phase     string
	Event     runner.Event
}

// PreparePhaseInputArgs is the input to PreparePhaseInput.
// Item is the per-iteration element when this phase belongs to a fanout child
// workflow; it is exposed to the prompt template as the .item variable so the
// child can render values like {{ .item.issue_number }}. It is nil for
// top-level (non-fanout) phases.
type PreparePhaseInputArgs struct {
	SessionID    uuid.UUID
	Phase        agentcfg.Phase
	ConfigName   string
	Runner       string
	PhaseOutputs map[string]any
	PromptDir    string
	Item         any
	StateBank    string
	// SubAgentParent and SubAgentKind identify a Case Supervisor cli sub-agent
	// so its stripped MCP secrets can be rehydrated (its own ConfigName is
	// empty). Both empty for a top-level external_agent task. See ExecInput.
	SubAgentParent string
	SubAgentKind   string
}

// ExecInput is the input to ExecAgentCLI.
type ExecInput struct {
	SessionID  uuid.UUID
	Runner     string
	Phase      string
	ConfigName string
	Config     runner.RunConfig
	// SubAgentParent is the parent task's config name and SubAgentKind is the
	// kind under that config's Supervisor.SubAgents, set only when this exec is
	// a Case Supervisor cli sub-agent. hydrateMCPSecrets uses them to find the
	// nested (unstripped) sub-agent config in the worker's AgentConfigs, since a
	// spawned sub-agent's own ConfigName is empty. Both empty for a top-level task.
	SubAgentParent string
	SubAgentKind   string
}

// ExecOutput is the result of ExecAgentCLI.
type ExecOutput struct {
	ExitCode      int
	TotalUsage    runner.TokenUsage
	TotalCostUSD  float64
	DurationMS    int64
	FinalText     string
	StructuredOut json.RawMessage
}

// ExternalAgentActivities is the struct-based activity bundle for the
// external_agent workflow.
type ExternalAgentActivities struct {
	Logger       *slog.Logger
	Runners      *runner.Registry
	EventSink    EventSink
	Store        SessionCreator
	AgentConfigs []agentcfg.AgentConfig
	Permits      PermitRegistrar
	PermAddress  string // Alfred HTTP server listen address (e.g. ":8080", "127.0.0.1:8080"); normalized via dialAddress before use as a dial target
	Guidance     GuidanceRegistrar
}

func (a *ExternalAgentActivities) log() *slog.Logger {
	if a.Logger != nil {
		return a.Logger
	}
	return slog.Default()
}

// SessionCreator is the subset of store.Store needed by CreateAgentSession.
type SessionCreator interface {
	CreateSession(ctx context.Context, sess *store.Session) error
	AppendMessagesAutoSeq(ctx context.Context, sessionID uuid.UUID, msgs []store.Message) error
}

// PrefilterCandidate is one opaque unit of work emitted by a prefilter
// command's "candidates" array. Key is the dedup identity used against the
// task_state ledger; Context is arbitrary JSON injected downstream verbatim.
// The engine never interprets either field's contents.
type PrefilterCandidate struct {
	Key     string          `json:"key"`
	Context json.RawMessage `json:"context,omitempty"`
}

// PrefilterResult is the parsed output of a prefilter command. Proceed/Data
// are the original proceed-or-no-work gate used by external_agent tasks.
// Candidates is the candidate-emitter extension used by queue_monitor tasks;
// an empty Candidates list means no candidate work this pass.
type PrefilterResult struct {
	Proceed    bool                 `json:"proceed"`
	Data       map[string]any       `json:"data,omitempty"`
	Candidates []PrefilterCandidate `json:"candidates,omitempty"`
}

// PreparePhaseInput renders the prompt and assembles the ExecInput for a phase.
// Runs as an activity so that file reads (os.ReadFile in RenderPrompt) and map
// iteration are recorded in workflow history, preserving Temporal determinism.
func (a *ExternalAgentActivities) PreparePhaseInput(_ context.Context, in PreparePhaseInputArgs) (ExecInput, error) { //nolint:gocritic // hugeParam: activity params are value-semantic for Temporal serialization
	vars := map[string]any{
		"prefilter": in.PhaseOutputs["prefilter"],
		"phases":    in.PhaseOutputs,
		"config":    map[string]any{"name": in.ConfigName, "runner": in.Runner},
		"item":      in.Item,
	}
	for k, v := range in.Phase.Prompt.Variables {
		vars[k] = v
	}
	prompt, err := RenderPrompt(in.PromptDir, in.Phase.Prompt, vars)
	if err != nil {
		return ExecInput{}, fmt.Errorf("prepare phase %s: %w", in.Phase.Name, err)
	}
	rc := runnerRunConfigFromPhase(in.Phase, prompt)
	if in.StateBank != "" {
		if rc.Env == nil {
			rc.Env = make(map[string]string)
		}
		rc.Env["HINDSIGHT_BANK"] = in.StateBank
	}
	return ExecInput{
		SessionID:      in.SessionID,
		Runner:         in.Runner,
		Phase:          in.Phase.Name,
		ConfigName:     in.ConfigName,
		Config:         rc,
		SubAgentParent: in.SubAgentParent,
		SubAgentKind:   in.SubAgentKind,
	}, nil
}

func (a *ExternalAgentActivities) RunPreflight(ctx context.Context, step agentcfg.PreflightStep) error {
	if step.Type != "bash" && step.Type != "" {
		return fmt.Errorf("unsupported preflight type %q", step.Type)
	}
	out, _, err := runBashCommand(ctx, step.Command, true)
	if err != nil {
		return fmt.Errorf("preflight %s: %w (output: %s)", step.Name, err, string(out))
	}
	a.log().Info("preflight ok", "name", step.Name, "output_len", len(out))
	return nil
}

func (a *ExternalAgentActivities) RunPrefilter(ctx context.Context, pf agentcfg.Prefilter) (PrefilterResult, error) {
	if pf.Type != "command" && pf.Type != "" {
		return PrefilterResult{}, fmt.Errorf("unsupported prefilter type %q", pf.Type)
	}
	stdout, stderr, err := runBashCommand(ctx, pf.Command, false)
	if err != nil {
		if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
			return PrefilterResult{}, fmt.Errorf("prefilter exit %d: %s", exitErr.ExitCode(), string(stderr))
		}
		return PrefilterResult{}, fmt.Errorf("prefilter run: %w", err)
	}

	var result PrefilterResult
	if err := json.Unmarshal(stdout, &result); err != nil {
		return PrefilterResult{}, fmt.Errorf("prefilter parse: %w (output: %s)", err, string(stdout))
	}
	return result, nil
}

// RunStateHook executes a State.OnComplete hook. Best-effort: errors are
// logged but never returned so the workflow outcome is not affected.
func (a *ExternalAgentActivities) RunStateHook(ctx context.Context, hook agentcfg.StateHook) error {
	if hook.Type != "bash" && hook.Type != "" {
		a.log().Warn("unsupported state hook type (ignored)", "type", hook.Type)
		return nil
	}
	out, _, err := runBashCommand(ctx, hook.Command, true)
	if err != nil {
		a.log().Warn("state hook failed (ignored)", "err", err, "output_len", len(out))
	}
	return nil
}

func (a *ExternalAgentActivities) RunCleanup(ctx context.Context, step agentcfg.CleanupStep) error {
	out, _, err := runBashCommand(ctx, step.Bash, true)
	if err != nil {
		a.log().Warn("cleanup step failed (ignored)", "err", err, "output_len", len(out))
	}
	return nil
}

// CreateAgentSessionArgs is the input to CreateAgentSession. ParentSessionID
// is set for fanout child workflows so the parent-child relationship is
// reconstructable from the sessions table; for top-level runs it is uuid.Nil
// and the FK column is left null. FanoutItem, when non-empty, is persisted as
// a context message with fanout_item metadata so the specific item this child
// processed is durable beyond the Temporal retention window.
type CreateAgentSessionArgs struct {
	SessionID       uuid.UUID
	WorkflowID      string
	RunID           string
	ParentSessionID uuid.UUID
	FanoutItem      json.RawMessage
}

// CreateAgentSession inserts a sessions row for an external agent run.
// Called as the first activity in ExternalAgentWorkflow so the FK constraint
// on messages.session_id is satisfied before any events are persisted.
// When FanoutItem is non-empty, a context message is appended with the item
// in metadata so the specific fanout element is durable.
func (a *ExternalAgentActivities) CreateAgentSession(ctx context.Context, args CreateAgentSessionArgs) error { //nolint:gocritic // hugeParam: activity params are value-semantic for Temporal serialization
	if a.Store == nil {
		return fmt.Errorf("store not configured")
	}
	var parent *uuid.UUID
	if args.ParentSessionID != uuid.Nil {
		pid := args.ParentSessionID
		parent = &pid
	}
	if err := a.Store.CreateSession(ctx, &store.Session{
		ID:            args.SessionID,
		WorkflowID:    args.WorkflowID,
		RunID:         args.RunID,
		ParentSession: parent,
		Kind:          store.SessionKindAgentTask,
		Status:        store.SessionStatusActive,
		Epoch:         1,
	}); err != nil && !store.IsConflict(err) {
		return err
	}

	if len(args.FanoutItem) > 0 {
		meta, err := json.Marshal(map[string]any{
			"event":       "session_start",
			"fanout_item": args.FanoutItem,
		})
		if err != nil {
			return fmt.Errorf("marshal fanout item metadata: %w", err)
		}
		if err := a.Store.AppendMessagesAutoSeq(ctx, args.SessionID, []store.Message{{
			ID:        uuid.New(),
			SessionID: args.SessionID,
			Role:      ctxbuild.RoleContext,
			Content:   "",
			Metadata:  meta,
		}}); err != nil {
			return fmt.Errorf("persist fanout item: %w", err)
		}
	}

	return nil
}

// ExecAgentCLI dispatches to a Runner, persists each event via the EventSink,
// and heartbeats per event so Temporal knows the activity is still alive.
func (a *ExternalAgentActivities) ExecAgentCLI(ctx context.Context, in ExecInput) (ExecOutput, error) { //nolint:gocognit,gocyclo,gocritic // gocognit/gocyclo: inherently-complex activity orchestrator (runner dispatch, per-event persistence, heartbeats, steering wiring); hugeParam: activity params are value-semantic for Temporal serialization
	if a.Runners == nil {
		return ExecOutput{}, fmt.Errorf("no runner registry configured")
	}
	rn, ok := a.Runners.Get(in.Runner)
	if !ok {
		return ExecOutput{}, fmt.Errorf("unknown runner: %s", in.Runner)
	}

	hydrateMCPSecrets(&in.Config, a.AgentConfigs, in.ConfigName, in.SubAgentParent, in.SubAgentKind, in.Phase)

	// For interactive steering, register a per-run token and inject the
	// alfred-perm MCP server into the run config.
	runID := in.SessionID.String()
	if in.Config.Steering == runner.SteeringInteractive && a.Permits != nil && a.PermAddress != "" {
		token := a.Permits.RegisterRun(runID, runRegistrationTTL)
		defer a.Permits.DeregisterRun(runID)

		if in.Config.MCP.Servers == nil {
			in.Config.MCP.Servers = make(map[string]runner.MCPServerConfig)
		}
		in.Config.MCP.Servers["alfred-perm"] = runner.MCPServerConfig{
			Type: "http",
			URL:  fmt.Sprintf("http://%s/mcp/perm/%s", dialAddress(a.PermAddress), runID),
			Headers: map[string]string{
				"Authorization": "Bearer " + token,
			},
		}
		in.Config.PermissionTool = "mcp__alfred-perm__permission_request"
	}

	// Mid-run guidance: register a channel and inject into RunConfig if the
	// runner supports BidirectionalStream. Non-bidi runners get no channel;
	// the workflow's inject_guidance update handler refuses the request
	// upfront for them, so we never need to wire one.
	if a.Guidance != nil && rn.Capabilities().BidirectionalStream {
		a.Guidance.RegisterRun(runID)
		defer a.Guidance.DeregisterRun(runID)
		if ch, ok := a.Guidance.Receiver(runID); ok {
			in.Config.Guidance = ch
		}
	}

	events := make(chan runner.Event, eventChannelBuffer)
	persistDone := make(chan struct{})

	go func() {
		defer close(persistDone)
		ticker := time.NewTicker(heartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case ev, ok := <-events:
				if !ok {
					return
				}
				activityHeartbeat(ctx, ev.Kind.String())
				if a.EventSink != nil {
					if err := a.EventSink.Persist(ctx, in.SessionID, in.Phase, ev); err != nil {
						a.log().Error("persist event failed", "err", err, "kind", ev.Kind.String())
					}
					a.EventSink.Broadcast(in.SessionID, ev)
				}
			case <-ticker.C:
				activityHeartbeat(ctx, "tick")
			}
		}
	}()

	defer func() {
		close(events)
		<-persistDone
	}()

	result, err := rn.Run(ctx, in.Config, events)

	if err != nil {
		return ExecOutput{}, err
	}
	return ExecOutput{
		ExitCode:      result.ExitCode,
		TotalUsage:    result.TotalUsage,
		TotalCostUSD:  result.TotalCostUSD,
		DurationMS:    result.DurationMS,
		FinalText:     result.FinalText,
		StructuredOut: result.StructuredOut,
	}, nil
}

// hydrateMCPSecrets restores Headers and Env on MCP server configs from the
// worker-local AgentConfigs. These fields are stripped before Temporal
// serialization and must be restored before the runner writes the MCP config
// file for the subprocess.
//
// The target config is resolved by identity: a top-level task by configName,
// or, for a Case Supervisor cli sub-agent (whose own AgentConfig has no
// top-level Name, it is keyed only by kind under the parent's
// Supervisor.SubAgents), by subAgentParent (the parent task's config name)
// plus subAgentKind. Sub-agent callers pass configName="" and set the two
// sub-agent fields; the parent config carried in a.AgentConfigs is unstripped,
// so its nested sub-agent still holds the real Headers/Env.
func hydrateMCPSecrets(cfg *runner.RunConfig, configs []agentcfg.AgentConfig, configName, subAgentParent, subAgentKind, phaseName string) {
	if len(cfg.MCP.Servers) == 0 {
		return
	}
	target := resolveHydrationConfig(configs, configName, subAgentParent, subAgentKind)
	if target == nil {
		slog.Debug("hydrateMCPSecrets: config not found",
			"config", configName, "sub_agent_parent", subAgentParent, "sub_agent_kind", subAgentKind)
		return
	}
	if !hydratePhaseMCP(cfg, target, phaseName) {
		slog.Debug("hydrateMCPSecrets: phase not found",
			"config", configName, "sub_agent_parent", subAgentParent, "sub_agent_kind", subAgentKind, "phase", phaseName)
	}
}

// resolveHydrationConfig locates the AgentConfig whose stripped MCP secrets
// should be restored: the top-level config named configName, or, when
// subAgentKind is non-empty, the Supervisor.SubAgents[subAgentKind] entry
// nested under the top-level config named subAgentParent. Returns nil when no
// match is found.
//
// The result is for READ-ONLY use: the top-level branch aliases the live slice
// element, but the sub-agent branch returns a pointer to a detached copy of the
// map value (Go map elements are not addressable), so a mutation through the
// pointer would silently not persist in the sub-agent case.
func resolveHydrationConfig(configs []agentcfg.AgentConfig, configName, subAgentParent, subAgentKind string) *agentcfg.AgentConfig {
	if subAgentKind != "" {
		for i := range configs {
			if configs[i].Name != subAgentParent {
				continue
			}
			sub, ok := configs[i].Supervisor.SubAgents[subAgentKind]
			if !ok {
				return nil
			}
			return &sub
		}
		return nil
	}
	for i := range configs {
		if configs[i].Name == configName {
			return &configs[i]
		}
	}
	return nil
}

// hydratePhaseMCP restores the stripped Headers/Env from target's matching
// phase into cfg's MCP servers, reporting whether a phase matched.
//
// An explicit phase is matched first, so a phase deliberately named like
// "<base>-<suffix>" is honored over the fanout fallback. Only when no phase
// matches exactly is the trailing "-N" stripped to resolve a fanout child
// (whose generated name is "<phase>-<index>", see buildChildInput) back to its
// parent phase, hydrating from the fanout child's own MCP servers. Without the
// exact-match-first pass, an explicit phase "foo-setup" alongside a fanout
// phase "foo" would be mishydrated from foo's fanout child.
func hydratePhaseMCP(cfg *runner.RunConfig, target *agentcfg.AgentConfig, phaseName string) bool {
	for j := range target.Phases {
		if target.Phases[j].Name == phaseName {
			copyMCPCreds(cfg, target.Phases[j].Tools.MCP.Servers)
			return true
		}
	}
	if idx := strings.LastIndex(phaseName, "-"); idx > 0 {
		base := phaseName[:idx]
		for j := range target.Phases {
			if target.Phases[j].Name == base && target.Phases[j].Fanout != nil {
				copyMCPCreds(cfg, target.Phases[j].Fanout.Child.Tools.MCP.Servers)
				return true
			}
		}
	}
	return false
}

// copyMCPCreds deep-copies the Headers and Env from each source MCP server
// into the matching server already present in cfg (by name), leaving a server
// with no source entry untouched.
func copyMCPCreds(cfg *runner.RunConfig, src map[string]agentcfg.MCPServerConfig) {
	for name, s := range src {
		if dst, ok := cfg.MCP.Servers[name]; ok {
			dst.Headers = maps.Clone(s.Headers)
			dst.Env = maps.Clone(s.Env)
			cfg.MCP.Servers[name] = dst
		}
	}
}

// activityHeartbeat is a package-level function overridden by init() in
// external_agent_heartbeat.go to wire in activity.RecordHeartbeat.
var activityHeartbeat = func(_ context.Context, _ ...any) {}
