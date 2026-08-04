// Package gemini implements the Gemini CLI runner adapter for Alfred's
// external_agent workflow.
package gemini

import (
	"context"
	"os"
	"strings"

	"github.com/tphakala/alfred/internal/agent/runner"
	"github.com/tphakala/alfred/internal/agent/runner/runnerutil"
)

// runnerName is the identifier this adapter registers under
// (AgentConfig.runner) and the default binary looked up on PATH.
const runnerName = "gemini"

// Config configures the Gemini runner.
type Config struct {
	BinaryPath string // defaults to "gemini" (PATH lookup)
}

// Runner is the Gemini adapter implementing runner.Runner.
type Runner struct {
	cfg Config
}

// New constructs a Runner. BinaryPath defaults to "gemini" if empty.
func New(cfg Config) *Runner {
	if cfg.BinaryPath == "" {
		cfg.BinaryPath = runnerName
	}
	return &Runner{cfg: cfg}
}

// Name reports the runner name used in AgentConfig.runner.
func (*Runner) Name() string { return runnerName }

// Capabilities reports what Gemini supports for steering and output.
// StructuredOutput is false until the runner populates RunResult.StructuredOut
// from the "result" event; advertising it as supported would mislead fanout
// phases into expecting a payload the runner never produces.
func (*Runner) Capabilities() runner.Capabilities {
	return runner.Capabilities{
		StructuredOutput:      false,
		PerCallPermissionHook: false,
		BidirectionalStream:   false,
		InCLISubAgents:        false,
		SandboxModes:          []string{"docker", "podman"},
		StructuredOutputJSON:  false,
	}
}

// Run spawns the gemini subprocess with stream-json output, parses each
// NDJSON line into a runner.Event, and forwards them over the events channel
// until the subprocess exits or the context is cancelled. The subprocess
// lifecycle is the shared runnerutil.RunPipeStreaming skeleton.
func (r *Runner) Run(ctx context.Context, cfg runner.RunConfig, events chan<- runner.Event) (runner.RunResult, error) { //nolint:gocritic // hugeParam: cfg is value-semantic per the runner.Runner interface
	env := scrubbedEnv(os.Environ(), cfg.HomeDir)
	for k, v := range cfg.Env {
		env = append(env, k+"="+v)
	}
	return runnerutil.RunPipeStreaming(ctx, &runnerutil.StreamSpec{
		Name:       runnerName,
		BinaryPath: r.cfg.BinaryPath,
		Args:       BuildArgs(cfg),
		Env:        env,
		ParseLine:  ParseLine,
		ParseDoneMetrics: func(line []byte) (runner.TokenUsage, float64, bool) {
			u, c, err := ParseDoneMetrics(line)
			return u, c, err == nil
		},
	}, events)
}

// scrubbedEnv returns os.Environ() with GEMINI_API_KEY and GOOGLE_API_KEY
// removed (auth precedence trap: setting them silently overrides OAuth Code
// Assist) and GEMINI_CLI_HOME set to homeDir when non-empty so each run gets
// an isolated state directory. Also sets GEMINI_TELEMETRY_ENABLED=false and
// NO_COLOR=1 for predictable subprocess output.
func scrubbedEnv(in []string, homeDir string) []string {
	scrub := map[string]struct{}{
		"GEMINI_API_KEY": {},
		"GOOGLE_API_KEY": {},
	}
	out := make([]string, 0, len(in)+3)
	for _, kv := range in {
		key, _, found := strings.Cut(kv, "=")
		if !found || key == "" {
			out = append(out, kv)
			continue
		}
		if _, drop := scrub[key]; drop {
			continue
		}
		out = append(out, kv)
	}
	if homeDir != "" {
		out = append(out, "GEMINI_CLI_HOME="+homeDir)
	}
	out = append(out, "GEMINI_TELEMETRY_ENABLED=false", "NO_COLOR=1")
	return out
}
