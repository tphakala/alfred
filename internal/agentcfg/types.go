package agentcfg

import (
	"strings"
	"time"
)

// Strategy values accepted by AgentConfig.Strategy and Validate.
const (
	StrategyExternalAgent = "external_agent"
	StrategyQueueMonitor  = "queue_monitor"
)

// Reserved tool names for the Case Supervisor's built-in orchestration
// tools. A DeclaredTool may not reuse either name: the engine presents these
// two tools to the supervisor LLM regardless of deployment config, so a
// deployment-declared command tool with the same name would be ambiguous.
const (
	ReservedToolSpawnAgent    = "spawn_agent"
	ReservedToolRecordOutcome = "record_outcome"
	// ReservedToolSubmitResult is the built-in terminal tool a native
	// sub-agent calls to return its schema-bound final output.
	ReservedToolSubmitResult = "submit_result"
	// ReservedToolReportFailure is the built-in terminal tool a native
	// sub-agent calls to give up explicitly with a reason, instead of
	// fabricating output to satisfy the submit_result schema or burning
	// rounds until a guard trips. A native DeclaredTool may reuse neither
	// native terminal name.
	ReservedToolReportFailure = "report_failure"
)

// Execution backends a sub-agent kind may run on. A sub-agent with no backend
// set is cli (backward compatible with pre-Phase-4 configs).
const (
	BackendCLI    = "cli"
	BackendNative = "native"
)

// AgentConfig is the YAML schema for a single external_agent or
// queue_monitor workflow.
type AgentConfig struct {
	Name        string          `yaml:"name"`
	Strategy    string          `yaml:"strategy"`
	Runner      string          `yaml:"runner"`
	Description string          `yaml:"description,omitempty"`
	Schedule    Schedule        `yaml:"schedule"`
	Concurrency Concurrency     `yaml:"concurrency"`
	Budgets     Budgets         `yaml:"budgets"`
	Preflight   []PreflightStep `yaml:"preflight"`
	Prefilter   Prefilter       `yaml:"prefilter"`
	Phases      []Phase         `yaml:"phases"`
	State       State           `yaml:"state"`
	Cleanup     []CleanupStep   `yaml:"cleanup"`
	Monitor     Monitor         `yaml:"monitor,omitempty"`
	Supervisor  Supervisor      `yaml:"supervisor,omitempty"`
	// Backend selects a sub-agent's execution backend: "cli" (default,
	// ExternalAgentWorkflow) or "native" (NativeSubAgentWorkflow). Only
	// meaningful when this AgentConfig is a supervisor sub-agent; ignored
	// for top-level queue_monitor / external_agent tasks.
	Backend string `yaml:"backend,omitempty"`
	// Native configures a backend: native sub-agent (its LLM loop model,
	// prompt, guard limits, declared command tools, and schema-bound
	// output). Unused when Backend is cli.
	Native NativeAgent `yaml:"native,omitempty"`
}

// LoopConfig holds the loop-shaping fields shared by the Case Supervisor and a
// native sub-agent, the two runAgentLoop parameterizations. It is embedded
// (yaml ",inline") in both Supervisor and NativeAgent so a new loop-shaping
// field (e.g. a future token budget) is declared once here instead of being
// hand-synced across both structs. Field promotion keeps every read site
// (sup.Model, na.Tools, ...) unchanged.
type LoopConfig struct {
	Model       string         `yaml:"model,omitempty"`
	Prompt      Prompt         `yaml:"prompt,omitempty"`
	MaxRounds   int            `yaml:"max_rounds,omitempty"`
	CostCapUSD  float64        `yaml:"cost_cap_usd,omitempty"`
	MaxDuration time.Duration  `yaml:"max_duration,omitempty"`
	Tools       []DeclaredTool `yaml:"tools,omitempty"`
}

// Supervisor is the Level-1 Case Supervisor configuration for a queue_monitor
// task: the LLM loop's model and prompt, its guard limits, the deployment-
// declared command tools it may call, and the sub-agent kinds it may spawn.
type Supervisor struct {
	LoopConfig `yaml:",inline"`
	SubAgents  map[string]AgentConfig `yaml:"sub_agents,omitempty"`
}

// NativeAgent is the config for a backend: native sub-agent: an autonomous LLM
// ReAct loop on Alfred's native providers. It mirrors the loop-shaping fields
// of Supervisor (model, prompt, guard limits, declared tools) via the shared
// LoopConfig, but terminates via submit_result (bound to Output.Schema) or
// report_failure rather than writing to the task_state ledger, and it spawns no
// further sub-agents. Output.Capture is ignored for native agents (it is a cli
// output-parsing concept); only Output.Schema matters here.
type NativeAgent struct {
	LoopConfig `yaml:",inline"`
	Output     Output `yaml:"output,omitempty"`
}

// BackendOf returns the execution backend for a sub-agent config, treating an
// empty Backend as cli so pre-Phase-4 configs (which never set it) keep
// dispatching to ExternalAgentWorkflow.
func BackendOf(cfg *AgentConfig) string {
	if cfg.Backend == "" {
		return BackendCLI
	}
	return cfg.Backend
}

// DeclaredTool is a deployment-declared command tool. It is presented to the
// supervisor LLM as an ordinary function declaration and executed by the
// generic RunDeclaredCommand activity. The engine never knows what the
// command does; it substitutes argument values as discrete argv elements
// (never shell).
type DeclaredTool struct {
	Name        string            `yaml:"name"`
	Description string            `yaml:"description,omitempty"`
	ArgsSchema  map[string]any    `yaml:"args_schema,omitempty"` // a JSON Schema object describing the tool's arguments
	Command     []string          `yaml:"command"`               // argv template; a whole element may be a "{placeholder}" naming an args_schema property; MUST contain a "--" element
	Idempotent  bool              `yaml:"idempotent,omitempty"`
	Env         map[string]string `yaml:"env,omitempty"` // values may be "${secret:NAME}", hydrated at exec time
}

// IsWholeElementPlaceholder reports whether elem is a whole-element command
// placeholder like "{name}": the entire element is a single pair of braces,
// with a non-empty name and no braces nested inside. An empty placeholder
// name ("{}") is not a valid placeholder and returns false, so a stray "{}"
// command element is rejected by config validation as embedded placeholder
// syntax instead of resolving to a zero-length argument lookup at exec time.
// This is the one definition of "valid placeholder" shared by config
// validation (which rejects embedded placeholders like "pre-{name}" or
// "{name}-suf") and command execution (RunDeclaredCommand substitutes only
// whole-element placeholders), so the two cannot silently drift apart.
func IsWholeElementPlaceholder(elem string) bool {
	if len(elem) < 2 || elem[0] != '{' || elem[len(elem)-1] != '}' {
		return false
	}
	inner := elem[1 : len(elem)-1]
	if inner == "" {
		return false
	}
	return !strings.ContainsAny(inner, "{}")
}

// Monitor holds the dispatch limits for a queue_monitor task: how many cases
// may run at once, how many new cases a single pass may start, and how many
// times a candidate may be re-dispatched before it is held as
// needs_attention.
type Monitor struct {
	MaxParallelCases   int `yaml:"max_parallel_cases"`
	MaxNewCasesPerPass int `yaml:"max_new_cases_per_pass"`
	MaxCaseAttempts    int `yaml:"max_case_attempts"`
}

type Schedule struct {
	Cron            string        `yaml:"cron,omitempty"`
	RandomizedDelay time.Duration `yaml:"randomized_delay,omitempty"`
	Persistent      bool          `yaml:"persistent,omitempty"`
}

type Concurrency struct {
	MaxConcurrent int    `yaml:"max_concurrent"`
	OnConflict    string `yaml:"on_conflict"`
}

type Budgets struct {
	WorkflowMaxCostUSD   float64       `yaml:"workflow_max_cost_usd,omitempty"`
	WorkflowMaxWallClock time.Duration `yaml:"workflow_max_wall_clock,omitempty"`
}

type PreflightStep struct {
	Name       string        `yaml:"name"`
	Type       string        `yaml:"type"`
	Command    string        `yaml:"command"`
	Timeout    time.Duration `yaml:"timeout,omitempty"`
	FailClosed bool          `yaml:"fail_closed"`
}

type Prefilter struct {
	Type    string        `yaml:"type"`
	Command string        `yaml:"command"`
	Timeout time.Duration `yaml:"timeout,omitempty"`
	Schema  string        `yaml:"schema,omitempty"`
}

type Phase struct {
	Name        string        `yaml:"name"`
	Model       string        `yaml:"model"`
	Effort      string        `yaml:"effort,omitempty"`
	RunnerFlags RunnerFlags   `yaml:"runner_flags,omitempty"`
	Prompt      Prompt        `yaml:"prompt"`
	Tools       Tools         `yaml:"tools,omitempty"`
	Steering    Steering      `yaml:"steering"`
	Output      Output        `yaml:"output"`
	Fanout      *FanoutConfig `yaml:"fanout,omitempty"`
}

type RunnerFlags struct {
	MaxTurns             int     `yaml:"max_turns,omitempty"`
	MaxBudgetUSD         float64 `yaml:"max_budget_usd,omitempty"`
	StrictMCPConfig      *bool   `yaml:"strict_mcp_config,omitempty"`
	Bare                 *bool   `yaml:"bare,omitempty"`
	NoSessionPersistence *bool   `yaml:"no_session_persistence,omitempty"`
}

type Prompt struct {
	Base      string            `yaml:"base"`
	Includes  []string          `yaml:"includes,omitempty"`
	Header    string            `yaml:"header,omitempty"`
	Variables map[string]string `yaml:"variables,omitempty"`
}

type Tools struct {
	Bash    []string `yaml:"bash,omitempty"`
	Builtin []string `yaml:"builtin,omitempty"`
	MCP     MCP      `yaml:"mcp,omitempty"`
}

type MCP struct {
	Servers map[string]MCPServerConfig `yaml:"servers,omitempty"`
}

// MCPServerConfig describes how to connect to an MCP server.
// Fields mirror Claude Code's MCP config schema.
type MCPServerConfig struct {
	Type    string            `yaml:"type,omitempty"`
	Command string            `yaml:"command,omitempty"`
	Args    []string          `yaml:"args,omitempty"`
	URL     string            `yaml:"url,omitempty"`
	Headers map[string]string `yaml:"headers,omitempty"`
	Env     map[string]string `yaml:"env,omitempty"`
}

// SteeringMode controls how the agent receives instructions.
type SteeringMode string

const (
	SteeringScheduled   SteeringMode = "scheduled"
	SteeringInteractive SteeringMode = "interactive"
)

type Steering struct {
	Mode SteeringMode `yaml:"mode"`
}

// OutputCapture controls how agent output is collected.
type OutputCapture string

const (
	OutputCaptureText       OutputCapture = "text"
	OutputCaptureStructured OutputCapture = "structured"
)

type Output struct {
	Capture OutputCapture `yaml:"capture"`
	Schema  any           `yaml:"schema,omitempty"`
}

type State struct {
	Bank       string      `yaml:"bank,omitempty"`
	OnComplete []StateHook `yaml:"on_complete,omitempty"`
}

type StateHook struct {
	Type    string `yaml:"type"`
	Command string `yaml:"command"`
	When    string `yaml:"when,omitempty"`
}

type CleanupStep struct {
	Bash string `yaml:"bash"`
}
