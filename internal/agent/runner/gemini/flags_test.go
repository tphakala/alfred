package gemini_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tphakala/alfred/internal/agent/runner"
	"github.com/tphakala/alfred/internal/agent/runner/gemini"
)

func TestBuildArgs_BasicScheduled(t *testing.T) {
	cfg := runner.RunConfig{
		Prompt:   "hello world",
		Model:    "gemini-3.1-pro-preview",
		Steering: runner.SteeringScheduled,
		Tools: runner.ToolPolicy{
			Bash:    []string{"git*"},
			Builtin: []string{"Read", "Grep"},
		},
		MCP: runner.MCPPolicy{
			Servers: map[string]runner.MCPServerConfig{
				"forgejo": {Type: "http", URL: "http://localhost:3000"},
			},
		},
	}

	args := gemini.BuildArgs(cfg)

	require.Contains(t, args, "-p")
	require.Contains(t, args, "hello world")
	require.Contains(t, args, "--output-format")
	idx := indexOf(args, "--output-format")
	assert.Equal(t, "stream-json", args[idx+1])
	require.Contains(t, args, "--approval-mode")
	idx = indexOf(args, "--approval-mode")
	assert.Equal(t, "yolo", args[idx+1])
	require.Contains(t, args, "--model")
	idx = indexOf(args, "--model")
	assert.Equal(t, "gemini-3.1-pro-preview", args[idx+1])
	require.Contains(t, args, "--allowed-tools")
	require.Contains(t, args, "--allowed-mcp-server-names")
}

func TestBuildArgs_InteractiveSteeringNotSupported(t *testing.T) {
	cfg := runner.RunConfig{
		Prompt:   "hello",
		Steering: runner.SteeringInteractive,
	}
	args := gemini.BuildArgs(cfg)

	// Gemini has no per-call permission hook. Even when the AgentConfig
	// requests interactive steering, the adapter falls back to yolo and
	// surfaces the gap via Capabilities.PerCallPermissionHook = false.
	idx := indexOf(args, "--approval-mode")
	require.Greater(t, idx, -1)
	assert.Equal(t, "yolo", args[idx+1])
}

func indexOf(args []string, target string) int {
	for i, a := range args {
		if a == target {
			return i
		}
	}
	return -1
}
