package claude

import (
	"context"
	"io"
	"os"
	"strings"

	"github.com/tphakala/alfred/internal/agent/runner"
	"github.com/tphakala/alfred/internal/agent/runner/runnerutil"
)

// runnerName is the identifier this adapter registers under
// (AgentConfig.runner) and the default binary looked up on PATH.
const runnerName = "claude"

// Config configures the Claude runner.
type Config struct {
	BinaryPath string
}

// Runner is the Claude adapter implementing runner.Runner.
type Runner struct {
	cfg Config
}

// New constructs a Runner. BinaryPath defaults to "claude" (PATH lookup) if empty.
func New(cfg Config) *Runner {
	if cfg.BinaryPath == "" {
		cfg.BinaryPath = runnerName
	}
	return &Runner{cfg: cfg}
}

// Name reports the runner name used in AgentConfig.runner.
func (*Runner) Name() string { return runnerName }

// Capabilities reports what Claude supports for steering and output.
func (*Runner) Capabilities() runner.Capabilities {
	return runner.Capabilities{
		StructuredOutput:      true,
		PerCallPermissionHook: true,
		BidirectionalStream:   true,
		InCLISubAgents:        true,
		StructuredOutputJSON:  true,
	}
}

// Run spawns the claude subprocess, parses each NDJSON line into a
// runner.Event, and forwards them over the events channel until the
// subprocess exits or the context is cancelled. The subprocess lifecycle is
// the shared runnerutil.RunPipeStreaming skeleton; claude adds the MCP config
// file and, when guidance is configured, the stdin steering stream.
func (r *Runner) Run(ctx context.Context, cfg runner.RunConfig, events chan<- runner.Event) (runner.RunResult, error) { //nolint:gocritic // hugeParam: cfg is value-semantic per the runner.Runner interface
	mcpPath, cleanupMCP, err := runnerutil.PrepareMCPConfig(cfg.HomeDir, cfg.MCP.Servers)
	if err != nil {
		return runner.RunResult{}, err
	}
	defer cleanupMCP()

	env := scrubbedEnv(os.Environ())
	for k, v := range cfg.Env {
		if _, blocked := envDenylist[k]; !blocked {
			env = append(env, k+"="+v)
		}
	}

	spec := runnerutil.StreamSpec{
		Name:       runnerName,
		BinaryPath: r.cfg.BinaryPath,
		Args:       BuildArgs(cfg, mcpPath),
		Env:        env,
		ParseLine:  ParseLine,
		ParseDoneMetrics: func(line []byte) (runner.TokenUsage, float64, bool) {
			u, c, jerr := ParseDoneMetrics(line)
			return u, c, jerr == nil
		},
	}
	if cfg.Guidance != nil {
		guidance := cfg.Guidance
		spec.Guidance = func(ctx context.Context, stdin io.Writer) {
			streamGuidance(ctx, stdin, guidance)
		}
	}
	return runnerutil.RunPipeStreaming(ctx, &spec, events)
}

var envDenylist = map[string]struct{}{
	"ANTHROPIC_API_KEY":    {},
	"ANTHROPIC_AUTH_TOKEN": {},
}

func scrubbedEnv(in []string) []string {
	out := make([]string, 0, len(in))
	for _, kv := range in {
		key, _, found := strings.Cut(kv, "=")
		if !found || key == "" {
			out = append(out, kv)
			continue
		}
		if _, drop := envDenylist[key]; drop {
			continue
		}
		out = append(out, kv)
	}
	return out
}
