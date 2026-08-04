package copilot_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tphakala/alfred/internal/agent/runner"
	"github.com/tphakala/alfred/internal/agent/runner/copilot"
)

func TestBuildArgs_BasicScheduled(t *testing.T) {
	cfg := runner.RunConfig{
		Prompt:   "do work",
		Model:    "claude-sonnet-4-6",
		Steering: runner.SteeringScheduled,
		Tools: runner.ToolPolicy{
			Bash: []string{"git*"},
		},
		HomeDir: "/tmp/run-x",
	}
	args := copilot.BuildArgs(cfg, "/tmp/run-x/copilot/mcp.json")

	require.Contains(t, args, "-p")
	require.Contains(t, args, "do work")
	require.Contains(t, args, "--output-format")
	idx := indexOf(args, "--output-format")
	assert.Equal(t, "json", args[idx+1])
	require.Contains(t, args, "--model")
	idx = indexOf(args, "--model")
	assert.Equal(t, "claude-sonnet-4-6", args[idx+1])
	require.Contains(t, args, "--allow-tool")
	require.Contains(t, args, "Bash(git*)")
	require.Contains(t, args, "--additional-mcp-config")
	idx = indexOf(args, "--additional-mcp-config")
	assert.Equal(t, "/tmp/run-x/copilot/mcp.json", args[idx+1])
	require.Contains(t, args, "-C")
	idx = indexOf(args, "-C")
	assert.Equal(t, "/tmp/run-x", args[idx+1])
}

func TestBuildArgs_NoMCPConfig_NoAdditionalFlag(t *testing.T) {
	cfg := runner.RunConfig{Prompt: "hi", Steering: runner.SteeringScheduled}
	args := copilot.BuildArgs(cfg, "")
	assert.NotContains(t, args, "--additional-mcp-config")
}

func indexOf(args []string, target string) int {
	for i, a := range args {
		if a == target {
			return i
		}
	}
	return -1
}
