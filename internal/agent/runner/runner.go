package runner

import (
	"context"
	"encoding/json"
)

// Runner spawns an agentic CLI subprocess, streams its native events through
// the events channel as normalized Event values, and returns when the
// subprocess exits.
type Runner interface {
	Name() string
	Capabilities() Capabilities
	Run(ctx context.Context, cfg RunConfig, events chan<- Event) (RunResult, error)
}

// Capabilities advertises which optional features a runner supports.
type Capabilities struct {
	StructuredOutput      bool
	PerCallPermissionHook bool
	BidirectionalStream   bool
	InCLISubAgents        bool
	SandboxModes          []string
	StructuredOutputJSON  bool
}

// SteeringMode mirrors AgentConfig.phases[].steering.mode.
type SteeringMode string

const (
	SteeringScheduled   SteeringMode = "scheduled"
	SteeringInteractive SteeringMode = "interactive"
)

// OutputCaptureMode mirrors AgentConfig.phases[].output.capture.
type OutputCaptureMode string

const (
	OutputCaptureText       OutputCaptureMode = "text"
	OutputCaptureStructured OutputCaptureMode = "structured"
)

// ToolPolicy is the runner-agnostic tool allowlist.
type ToolPolicy struct {
	Bash    []string
	Builtin []string
}

// MCPServerConfig describes how to connect to an MCP server.
type MCPServerConfig struct {
	Type    string            `json:"type,omitempty"`
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
}

// MCPPolicy describes which MCP servers are exposed to the runner.
type MCPPolicy struct {
	Servers map[string]MCPServerConfig
}

// RunConfig is the rendered, ready-to-execute input to Runner.Run.
type RunConfig struct {
	Model                string
	Prompt               string
	Tools                ToolPolicy
	MCP                  MCPPolicy
	Steering             SteeringMode
	PermissionTool       string
	HomeDir              string
	MaxTurns             int
	MaxBudgetUSD         float64
	OutputCapture        OutputCaptureMode
	OutputSchema         []byte
	Bare                 bool
	StrictMCPConfig      bool
	NoSessionPersistence bool
	Env                  map[string]string
	// Guidance receives user-message text to inject mid-run. Runners that
	// advertise Capabilities.BidirectionalStream consume from this channel and
	// write each message into the subprocess's stdin between assistant turns.
	// nil for runs without mid-run guidance (the default).
	// The json:"-" tag excludes this channel from Temporal activity serialization;
	// it is a runtime-only field wired directly by ExecAgentCLI before calling Run.
	Guidance <-chan string `json:"-"`
}

// EventKind enumerates the normalized event types emitted by all runners.
type EventKind int

const (
	KindUnknown EventKind = iota
	KindSessionStart
	KindTextDelta
	KindToolCall
	KindToolResult
	KindApiRetry
	KindError
	KindStructuredOutput
	KindDone
	KindReplay
)

func (k EventKind) String() string {
	switch k {
	case KindSessionStart:
		return "session_start"
	case KindTextDelta:
		return "text_delta"
	case KindToolCall:
		return "tool_call"
	case KindToolResult:
		return "tool_result"
	case KindApiRetry:
		return "api_retry"
	case KindError:
		return "error"
	case KindStructuredOutput:
		return "structured_output"
	case KindDone:
		return "done"
	case KindReplay:
		return "replay"
	default:
		return "unknown"
	}
}

// MarshalJSON serializes EventKind as its string form.
func (k EventKind) MarshalJSON() ([]byte, error) { return json.Marshal(k.String()) }

// UnmarshalJSON parses EventKind from its string form.
func (k *EventKind) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	switch s {
	case "session_start":
		*k = KindSessionStart
	case "text_delta":
		*k = KindTextDelta
	case "tool_call":
		*k = KindToolCall
	case "tool_result":
		*k = KindToolResult
	case "api_retry":
		*k = KindApiRetry
	case "error":
		*k = KindError
	case "structured_output":
		*k = KindStructuredOutput
	case "done":
		*k = KindDone
	case "replay":
		*k = KindReplay
	default:
		*k = KindUnknown
	}
	return nil
}

// TokenUsage is per-event or cumulative usage accounting.
type TokenUsage struct {
	InputTokens  int `json:"input_tokens,omitempty"`
	OutputTokens int `json:"output_tokens,omitempty"`
	TotalTokens  int `json:"total_tokens,omitempty"`
}

// Event is the normalized stream element emitted by every runner adapter.
type Event struct {
	Kind       EventKind       `json:"kind"`
	Round      int             `json:"round,omitempty"`
	CallID     string          `json:"call_id,omitempty"`
	Tool       string          `json:"tool,omitempty"`
	Args       json.RawMessage `json:"args,omitempty"`
	Result     json.RawMessage `json:"result,omitempty"`
	Text       string          `json:"text,omitempty"`
	UsageDelta TokenUsage      `json:"usage_delta,omitempty"`
	Raw        json.RawMessage `json:"raw,omitempty"`
}

// RunResult is returned by Runner.Run after the subprocess exits.
type RunResult struct {
	ExitCode      int
	TotalUsage    TokenUsage
	TotalCostUSD  float64
	DurationMS    int64
	FinalText     string
	StructuredOut json.RawMessage
}
