package copilot

import (
	"fmt"

	"github.com/tphakala/alfred/internal/agent/runner"
)

// BuildArgs constructs the CLI argv (excluding argv[0]) for a copilot -p
// invocation matching the given RunConfig. mcpConfigPath is the path to the
// per-run MCP config file Alfred wrote, or empty if none is needed.
func BuildArgs(cfg runner.RunConfig, mcpConfigPath string) []string {
	args := []string{
		"-p", cfg.Prompt,
		"--output-format", "json",
	}

	if cfg.Model != "" {
		args = append(args, "--model", cfg.Model)
	}

	for _, p := range cfg.Tools.Bash {
		args = append(args, "--allow-tool", fmt.Sprintf("Bash(%s)", p))
	}
	for _, b := range cfg.Tools.Builtin {
		args = append(args, "--allow-tool", b)
	}

	if mcpConfigPath != "" {
		args = append(args, "--additional-mcp-config", mcpConfigPath)
	}

	if cfg.HomeDir != "" {
		args = append(args, "-C", cfg.HomeDir)
	}

	return args
}
