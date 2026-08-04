package claude

import (
	"fmt"
	"sort"
	"strconv"

	"github.com/tphakala/alfred/internal/agent/runner"
)

// BuildArgs constructs the CLI argv (excluding argv[0]) for a claude -p
// invocation matching the given RunConfig. mcpConfigPath is the path to the
// per-run MCP config file Alfred wrote, or empty if no MCP config is needed.
func BuildArgs(cfg runner.RunConfig, mcpConfigPath string) []string {
	args := []string{
		"-p", cfg.Prompt,
		"--output-format", "stream-json",
		"--verbose",
		"--include-partial-messages",
	}

	if cfg.Guidance != nil {
		args = append(args, "--input-format", "stream-json", "--replay-user-messages")
	}

	if cfg.Bare {
		args = append(args, "--bare")
	}
	if cfg.NoSessionPersistence {
		args = append(args, "--no-session-persistence")
	}

	if cfg.Model != "" {
		args = append(args, "--model", cfg.Model)
	}
	if cfg.MaxTurns > 0 {
		args = append(args, "--max-turns", strconv.Itoa(cfg.MaxTurns))
	}
	if cfg.MaxBudgetUSD > 0 {
		args = append(args, "--max-budget-usd", fmt.Sprintf("%.2f", cfg.MaxBudgetUSD))
	}

	if mcpConfigPath != "" {
		if cfg.StrictMCPConfig {
			args = append(args, "--strict-mcp-config")
		}
		args = append(args, "--mcp-config", mcpConfigPath)
	}

	allowed := buildAllowedTools(cfg.Tools, cfg.MCP)
	if len(allowed) > 0 {
		args = append(args, "--allowedTools")
		args = append(args, allowed...)
	}

	switch cfg.Steering {
	case runner.SteeringInteractive:
		if cfg.PermissionTool != "" {
			args = append(args, "--permission-prompt-tool", cfg.PermissionTool)
		}
	case runner.SteeringScheduled:
		fallthrough
	default:
		args = append(args, "--permission-mode", "dontAsk")
	}

	return args
}

func buildAllowedTools(tp runner.ToolPolicy, mcp runner.MCPPolicy) []string {
	out := make([]string, 0, len(tp.Bash)+len(tp.Builtin)+len(mcp.Servers))
	for _, p := range tp.Bash {
		out = append(out, fmt.Sprintf("Bash(%s)", p))
	}
	out = append(out, tp.Builtin...)
	mcpStart := len(out)
	for name := range mcp.Servers {
		out = append(out, fmt.Sprintf("mcp__%s__*", name))
	}
	sort.Strings(out[mcpStart:])
	return out
}
