package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"

	"github.com/google/uuid"

	"github.com/tphakala/alfred/internal/agentcfg"
	"github.com/tphakala/alfred/internal/llm"
	"github.com/tphakala/alfred/internal/secrets"
	"github.com/tphakala/alfred/internal/store"
)

// CaseActivities holds dependencies for Case Supervisor activities.
// Registered with the Temporal worker; each method is an activity. Unlike
// ChatActivities, it carries no baked-in system prompt or hardcoded model:
// the supervisor's system instruction and model come entirely from the
// deployment config via the workflow. Context building, round persistence,
// and SSE emission for case sessions deliberately reuse
// ChatActivities.BuildContext / PersistRound / EmitSSE instead of being
// duplicated here: those three activities take only a session ID, token
// budget, epoch summary, or a persist/event payload, with no chat-specific
// coupling, so a case session can call them by name on the same worker.
type CaseActivities struct {
	Store         *store.Store
	SessionBroker SessionEventPublisher
	Logger        *slog.Logger
	LLM           llm.Client
	DefaultModel  string            // fallback model when a case config sets no supervisor model
	Secrets       map[string]string // secret NAME -> value, for RunDeclaredCommand env hydration
	// Pricing converts a round's token usage to USD so the agent loop can meter
	// its own LLM spend against cost_cap_usd. Nil disables metering (spend then
	// counts as zero, bounded only by max_rounds/max_duration).
	Pricing llm.PriceTable

	// unpricedModelWarned dedupes the "model has no pricing" warning to once per
	// model over the worker's life, so an unpriced model does not log every
	// round.
	unpricedModelWarned sync.Map
}

// CaseLLMStreamRequest is the input for the CaseLLMStream activity. It
// embeds the same context-message and tool-config types chat's LLMStream
// uses, but carries its own Model (the workflow renders the supervisor's
// configured model here) since CaseActivities has no equivalent of
// ChatActivities.ChatModel to fall back to implicitly.
type CaseLLMStreamRequest struct {
	SessionID         uuid.UUID
	TurnID            int
	Round             int
	Messages          []ContextMessage
	Tools             []*llm.FunctionDeclaration
	ToolConfig        *llm.ToolConfig
	SystemInstruction string
	Model             string
}

// CaseLLMStreamResult is the output of the CaseLLMStream activity.
type CaseLLMStreamResult struct {
	Text      string
	ToolCalls []LLMToolCall
	Usage     *StreamTokenUsage
	// CostUSD is this round's LLM spend in USD, derived from Usage via the
	// activity's pricing table. Zero when usage is unavailable or the model is
	// unpriced. The agent loop accumulates it into the cost-cap total. Computed
	// in the activity (not the workflow) so it is recorded in Temporal history
	// and stays stable across replay even if pricing changes between deploys.
	CostUSD float64
}

// CaseLLMStream streams one LLM round for the Case Supervisor loop. It
// mirrors ChatActivities.LLMStream (same streaming loop, same "chunk" SSE
// events, same t{round}-c{index} CallID scheme via runLLMStreamLoop) with two
// deliberate differences:
//
//   - No baked-in system prompt is prepended. The system instruction comes
//     entirely from req.SystemInstruction, which the workflow sets to the
//     rendered supervisor prompt; chat's LLMStream prepends its configured
//     ChatActivities.SystemPrompt, but the supervisor has no such thing.
//   - The model comes from req.Model, falling back to a.DefaultModel when
//     empty, rather than a hardcoded chat model.
func (a *CaseActivities) CaseLLMStream(ctx context.Context, req CaseLLMStreamRequest) (*CaseLLMStreamResult, error) { //nolint:gocritic // hugeParam: activity params are value-semantic for Temporal serialization
	if a.LLM == nil {
		return nil, fmt.Errorf("CaseLLMStream: LLM client is nil")
	}

	messages := make([]llm.Message, len(req.Messages))
	for i, m := range req.Messages {
		messages[i] = llm.Message{Role: llm.Role(m.Role), Text: m.Content}
	}

	model := req.Model
	if model == "" {
		model = a.DefaultModel
	}

	streamReq := llm.StreamRequest{
		Model:             model,
		Messages:          messages,
		Tools:             req.Tools,
		ToolConfig:        req.ToolConfig,
		SystemInstruction: req.SystemInstruction,
	}

	iter, err := a.LLM.Stream(ctx, streamReq)
	if err != nil {
		return nil, fmt.Errorf("CaseLLMStream: stream start: %w", err)
	}
	defer func() { _ = iter.Close() }()

	text, toolCalls, usage, err := runLLMStreamLoop(ctx, iter, req.Round, req.TurnID, func(eventType string, data map[string]any) {
		a.emitSessionEvent(req.SessionID, eventType, data)
	})
	if err != nil {
		return nil, fmt.Errorf("CaseLLMStream: %w", err)
	}

	return &CaseLLMStreamResult{
		Text:      text,
		ToolCalls: toolCalls,
		Usage:     usage,
		CostUSD:   a.priceRound(model, usage),
	}, nil
}

// priceRound converts a round's token usage to USD via the activity's pricing
// table so the agent loop can meter its own LLM spend against cost_cap_usd. It
// returns 0 when usage is unavailable (the provider reported no token counts)
// or no pricing is configured. An unpriced model (pricing is set but has no
// entry for this model) returns 0 and is logged once per model, so the operator
// can add pricing rather than have cost_cap silently miss that model's spend.
func (a *CaseActivities) priceRound(model string, usage *StreamTokenUsage) float64 {
	if usage == nil || len(a.Pricing) == 0 {
		return 0
	}
	usd, known := a.Pricing.CostUSD(model, int(usage.PromptTokens), int(usage.ResponseTokens))
	if !known {
		if _, warned := a.unpricedModelWarned.LoadOrStore(model, struct{}{}); !warned && a.Logger != nil {
			a.Logger.Warn("case supervisor: model has no pricing entry; its token spend will not count toward cost_cap_usd",
				"model", model)
		}
		return 0
	}
	return usd
}

// emitSessionEvent publishes an SSE event to session subscribers. See
// publishSessionEvent (chat_activities.go) for why this is a thin delegate
// rather than a duplicate implementation: CaseActivities and ChatActivities
// intentionally share no embedding, so each gets its own tiny method with
// the shared logic factored out underneath.
func (a *CaseActivities) emitSessionEvent(sessionID uuid.UUID, eventType string, data map[string]any) {
	publishSessionEvent(a.SessionBroker, a.Logger, sessionID, eventType, data)
}

// RenderCasePrompt renders the Case Supervisor's system instruction from the
// deployment-declared prompt config. Runs as an activity because RenderPrompt
// reads prompt files from disk (os.ReadFile), which is non-deterministic and
// must not run inline in workflow code.
func (a *CaseActivities) RenderCasePrompt(_ context.Context, promptDir string, p agentcfg.Prompt, vars map[string]any) (string, error) { //nolint:gocritic // hugeParam: activity params are value-semantic for Temporal serialization
	return RenderPrompt(promptDir, p, vars)
}

// RunDeclaredCommandArgs is the input for the RunDeclaredCommand activity.
type RunDeclaredCommandArgs struct {
	Tool agentcfg.DeclaredTool
	Args map[string]any // the LLM-provided argument values for this call
}

// RunDeclaredCommandResult is the output of the RunDeclaredCommand activity.
// A rejected call (missing placeholder value, injection guard trip) is
// reported here with a non-zero ExitCode and an explanatory Stderr, not as
// an activity error: the supervisor LLM needs to see why its call was
// rejected so it can retry with corrected arguments. ExitCode -1 means the
// command was never executed (rejected before exec, or exec was cancelled).
type RunDeclaredCommandResult struct {
	ExitCode int
	Stdout   string
	Stderr   string
}

// rejectedResult builds a RunDeclaredCommandResult for a call that was
// rejected before exec: ExitCode -1 (never ran) and reason in Stderr, so the
// supervisor LLM sees the rejection as an ordinary tool result it can react
// to instead of a Temporal activity failure.
func rejectedResult(reason string) (RunDeclaredCommandResult, error) {
	return RunDeclaredCommandResult{ExitCode: -1, Stderr: reason}, nil
}

// RunDeclaredCommand executes a deployment-declared command tool safely. It
// is never routed through a shell: argument values are substituted as
// discrete argv elements and exec'd directly via runArgvCommand.
//
// Two classes of failure are distinguished:
//   - A malformed Tool.Command (missing the mandatory "--" terminator, or an
//     unresolvable ${secret:...} reference) is a deployment configuration
//     defect. Validate should already reject these at load time, but this
//     activity defends in depth and returns a real error so Temporal's retry
//     policy and the workflow's failure handling see it as an actual
//     failure, not something the LLM can fix by retrying with different
//     arguments.
//   - A bad call from the LLM this round (a missing placeholder value, or a
//     pre-"--" value that looks like a flag) is encoded in the result
//     (rejectedResult) so the LLM can see what was wrong and retry with
//     corrected arguments in the next round.
//
// A config-defect error does not hard-fail the case: case.go's
// runDeclaredTools converts any activity error into an ordinary error
// tool-result string fed back to the supervisor LLM, the same as a rejected
// call. A persistent config defect therefore just burns rounds until
// max_rounds or max_duration finalizes the case needs_attention; it is not a
// hard case failure.
//
// A non-zero process exit is captured in the result with a nil error; only a
// failure to start the process, or the exec being cancelled mid-flight,
// returns a real error (see runArgvCommand).
func (a *CaseActivities) RunDeclaredCommand(ctx context.Context, in RunDeclaredCommandArgs) (RunDeclaredCommandResult, error) { //nolint:gocritic // hugeParam: activity params are value-semantic for Temporal serialization
	dashIdx := locateFlagTerminator(in.Tool.Command)
	if dashIdx < 0 {
		return RunDeclaredCommandResult{}, fmt.Errorf("RunDeclaredCommand: tool %q: command has no \"--\" flag terminator", in.Tool.Name)
	}
	// Defense in depth: Validate already rejects a whole-element placeholder
	// at command[0] (see agentcfg.validateDeclaredTool), but a config that
	// bypassed validation must not hand the choice of executable to the LLM.
	if agentcfg.IsWholeElementPlaceholder(in.Tool.Command[0]) {
		return RunDeclaredCommandResult{}, fmt.Errorf("RunDeclaredCommand: tool %q: command[0] (the executable) is a placeholder, not a literal", in.Tool.Name)
	}

	argv, err := substituteDeclaredCommandArgv(&in.Tool, in.Args, dashIdx)
	if err != nil {
		if rejection, ok := errors.AsType[*declaredCommandRejectionError](err); ok {
			return rejectedResult(rejection.reason)
		}
		return RunDeclaredCommandResult{}, fmt.Errorf("RunDeclaredCommand: tool %q: %w", in.Tool.Name, err)
	}

	env, err := buildDeclaredCommandEnv(in.Tool.Env, a.Secrets)
	if err != nil {
		return RunDeclaredCommandResult{}, fmt.Errorf("RunDeclaredCommand: tool %q: %w", in.Tool.Name, err)
	}

	stdout, stderr, exitCode, err := runArgvCommand(ctx, argv, env)
	if err != nil {
		return RunDeclaredCommandResult{}, fmt.Errorf("RunDeclaredCommand: tool %q: %w", in.Tool.Name, err)
	}

	// Truncate here, before the result crosses into Temporal history: stdout
	// and stderr can each be up to the 1 MiB subprocess cap (runArgvCommand),
	// so an untruncated result could approach Temporal's blob-size limit and
	// wedge the case. formatDeclaredCommandResult applies the same bound
	// again on the JSON-marshaled payload, but that is too late to keep the
	// raw bytes out of history.
	return RunDeclaredCommandResult{
		ExitCode: exitCode,
		Stdout:   truncateString(string(stdout)),
		Stderr:   truncateString(string(stderr)),
	}, nil
}

// locateFlagTerminator returns the index of the first "--" element in
// command, or -1 if none is present.
func locateFlagTerminator(command []string) int {
	for i, elem := range command {
		if elem == "--" {
			return i
		}
	}
	return -1
}

// declaredCommandRejectionError marks a per-call substitution failure that must be
// reported to the supervisor LLM as a normal tool result (a rejected call it
// can retry with corrected arguments), not as a Temporal activity error. See
// RunDeclaredCommand's doc comment for the full failure-class distinction.
type declaredCommandRejectionError struct{ reason string }

func (r *declaredCommandRejectionError) Error() string { return r.reason }

// substituteDeclaredCommandArgv walks tool.Command, replacing each
// whole-element placeholder with the string form of the matching value in
// args as a single argv element (never split, never shell-quoted). A literal
// element, including the "--" terminator itself, passes through unchanged.
//
// Returns a *declaredCommandRejectionError for a per-call problem (a missing
// placeholder value, or a pre-"--" value that looks like a flag): the caller
// reports these to the LLM as a rejected result rather than failing the
// activity. Any other error is a deployment configuration defect (an
// embedded placeholder the validator should already have caught).
func substituteDeclaredCommandArgv(tool *agentcfg.DeclaredTool, args map[string]any, dashIdx int) ([]string, error) {
	argv := make([]string, 0, len(tool.Command))
	for i, elem := range tool.Command {
		if elem == "--" {
			argv = append(argv, elem)
			continue
		}
		if !agentcfg.IsWholeElementPlaceholder(elem) {
			if strings.ContainsAny(elem, "{}") {
				return nil, fmt.Errorf("command element %q has embedded placeholder syntax", elem)
			}
			argv = append(argv, elem)
			continue
		}

		name := elem[1 : len(elem)-1]
		val, ok := args[name]
		if !ok {
			return nil, &declaredCommandRejectionError{reason: fmt.Sprintf("rejected: missing value for placeholder %q", elem)}
		}
		strVal := stringifyArg(val)

		// Injection guard: a pre-"--" placeholder value must not look like a
		// flag. Values after "--" may be anything (see DeclaredTool docs).
		// Leading/trailing whitespace is trimmed only for this check, so a
		// value like " -f" cannot slip past a downstream tool that trims
		// before parsing its own flags; the substituted argv element itself
		// keeps the original, untrimmed strVal.
		if i < dashIdx && strings.HasPrefix(strings.TrimSpace(strVal), "-") {
			return nil, &declaredCommandRejectionError{reason: fmt.Sprintf(
				"rejected: value for %q begins with \"-\" before the \"--\" terminator; potential flag injection", elem)}
		}
		argv = append(argv, strVal)
	}
	return argv, nil
}

// buildDeclaredCommandEnv builds the child process environment: the current
// process's environment overlaid with toolEnv, resolving any ${secret:...}
// reference against secretsMap. Fails closed: an unresolved secret reference
// returns an error rather than running the command without the declared
// credential.
func buildDeclaredCommandEnv(toolEnv, secretsMap map[string]string) ([]string, error) {
	env := os.Environ()
	for k, v := range toolEnv {
		val := v
		if secrets.IsSecretRef(v) {
			name := secrets.ExtractSecretName(v)
			resolved, ok := secretsMap[name]
			if !ok {
				return nil, fmt.Errorf("secret %q not found", name)
			}
			val = resolved
		}
		env = append(env, k+"="+val)
	}
	return env, nil
}

// stringifyArg converts an LLM-provided argument value to a single argv
// element. Scalars use their natural string form; a map or slice is
// JSON-marshaled so structured values still cross as one argv element
// instead of being mangled by fmt's default formatting.
func stringifyArg(val any) string {
	switch v := val.(type) {
	case string:
		return v
	case nil:
		return ""
	case map[string]any, []any:
		if b, err := json.Marshal(v); err == nil {
			return string(b)
		}
	}
	return fmt.Sprint(val)
}
