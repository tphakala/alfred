// Package copilot implements the GitHub Copilot CLI runner adapter for
// Alfred's external_agent workflow.
package copilot

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"

	"github.com/tphakala/alfred/internal/agent/runner"
	"github.com/tphakala/alfred/internal/agent/runner/runnerutil"
)

// runnerName is the identifier this adapter registers under
// (AgentConfig.runner) and the default binary looked up on PATH.
const runnerName = "copilot"

// Config configures the Copilot runner.
type Config struct {
	BinaryPath string
}

// Runner is the Copilot adapter implementing runner.Runner.
type Runner struct {
	cfg Config
}

// New constructs a Runner. BinaryPath defaults to "copilot" (PATH lookup) if empty.
func New(cfg Config) *Runner {
	if cfg.BinaryPath == "" {
		cfg.BinaryPath = runnerName
	}
	return &Runner{cfg: cfg}
}

// Name reports the runner name used in AgentConfig.runner.
func (*Runner) Name() string { return runnerName }

// Capabilities reports what Copilot supports. StructuredOutput is false until
// the runner populates RunResult.StructuredOut from the "complete" event;
// advertising it as supported would mislead fanout phases into expecting a
// payload the runner never produces.
func (*Runner) Capabilities() runner.Capabilities {
	return runner.Capabilities{
		StructuredOutput:      false,
		PerCallPermissionHook: false,
		BidirectionalStream:   false,
		StructuredOutputJSON:  false,
	}
}

// Run spawns the copilot subprocess with JSON output, parses each JSONL line
// into a runner.Event, and forwards them over the events channel until the
// subprocess exits or the context is cancelled. The subprocess lifecycle is
// the shared runnerutil.RunPipeStreaming skeleton.
//
// When the RunConfig declares MCP servers, runnerutil.PrepareMCPConfig
// writes a per-run mcp.json into cfg.HomeDir (or a fresh temp directory if
// HomeDir is empty) and BuildArgs receives its absolute path so the
// subprocess is launched with --additional-mcp-config.
func (r *Runner) Run(ctx context.Context, cfg runner.RunConfig, events chan<- runner.Event) (runner.RunResult, error) { //nolint:gocritic // hugeParam: cfg is value-semantic per the runner.Runner interface
	mcpPath, cleanupMCP, err := runnerutil.PrepareMCPConfig(cfg.HomeDir, cfg.MCP.Servers)
	if err != nil {
		return runner.RunResult{}, err
	}
	defer cleanupMCP()

	env := buildEnv(os.Environ(), cfg.HomeDir)
	for k, v := range cfg.Env {
		env = append(env, k+"="+v)
	}

	// Copilot's done envelope carries token usage but no cost figure, so the
	// shared result's TotalCostUSD stays zero, matching the pre-consolidation
	// RunResult shape.
	return runnerutil.RunPipeStreaming(ctx, &runnerutil.StreamSpec{
		Name:       runnerName,
		BinaryPath: r.cfg.BinaryPath,
		Args:       BuildArgs(cfg, mcpPath),
		Env:        env,
		ParseLine:  ParseLine,
		ParseDoneMetrics: func(line []byte) (runner.TokenUsage, float64, bool) {
			var d doneEnvelope
			if jerr := json.Unmarshal(line, &d); jerr != nil {
				return runner.TokenUsage{}, 0, false
			}
			return d.Usage, 0, true
		},
	}, events)
}

// buildEnv constructs the subprocess environment for a copilot run. It adds
// the Copilot-specific env vars (COPILOT_HOME, COPILOT_CLI, etc.) and lazily
// resolves GH_TOKEN via `gh auth token` if it is not already in the inherited
// env. Errors from `gh auth token` are swallowed; if no token is available,
// the subprocess is launched without one and copilot will surface its own
// auth error.
func buildEnv(in []string, homeDir string) []string {
	out := append([]string{}, in...)
	if homeDir != "" {
		out = append(out, "COPILOT_HOME="+homeDir)
	}
	out = append(out,
		"COPILOT_CLI=1",
		"COPILOT_DISABLE_TERMINAL_TITLE=1",
		"COPILOT_PLUGIN_DIR_ONLY=1",
	)
	if _, found := lookupEnv(in, "GH_TOKEN"); !found {
		token, err := readGHAuthToken()
		if err == nil && token != "" {
			out = append(out, "GH_TOKEN="+token)
		}
	}
	return out
}

func readGHAuthToken() (string, error) {
	cmd := exec.Command("gh", "auth", "token")
	b, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

func lookupEnv(env []string, key string) (string, bool) {
	prefix := key + "="
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			return strings.TrimPrefix(kv, prefix), true
		}
	}
	return "", false
}
