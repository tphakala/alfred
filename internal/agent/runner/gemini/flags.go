package gemini

import (
	"fmt"
	"sort"
	"strings"

	"github.com/tphakala/alfred/internal/agent/runner"
)

// BuildArgs constructs the CLI argv (excluding argv[0]) for a gemini -p
// invocation matching the given RunConfig.
func BuildArgs(cfg runner.RunConfig) []string {
	args := []string{
		"-p", cfg.Prompt,
		"--output-format", "stream-json",
		"--approval-mode", "yolo",
	}

	if cfg.Model != "" {
		args = append(args, "--model", cfg.Model)
	}

	if tools := allowedTools(cfg.Tools); tools != "" {
		args = append(args, "--allowed-tools", tools)
	}

	if mcpNames := allowedMCPServers(cfg.MCP); mcpNames != "" {
		args = append(args, "--allowed-mcp-server-names", mcpNames)
	}

	if cfg.HomeDir != "" {
		args = append(args, "--include-directories", cfg.HomeDir)
	}

	return args
}

func allowedTools(tp runner.ToolPolicy) string {
	out := make([]string, 0, len(tp.Bash)+len(tp.Builtin))
	for _, p := range tp.Bash {
		out = append(out, fmt.Sprintf("Bash(%s)", p))
	}
	out = append(out, tp.Builtin...)
	if len(out) == 0 {
		return ""
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

func allowedMCPServers(mcp runner.MCPPolicy) string {
	if len(mcp.Servers) == 0 {
		return ""
	}
	names := make([]string, 0, len(mcp.Servers))
	for name := range mcp.Servers {
		names = append(names, name)
	}
	sort.Strings(names)
	return strings.Join(names, ",")
}
