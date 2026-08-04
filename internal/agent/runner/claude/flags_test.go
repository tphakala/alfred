package claude

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/tphakala/alfred/internal/agent/runner"
)

func TestBuildArgs_MinimalScheduled(t *testing.T) {
	cfg := runner.RunConfig{
		Model:                "claude-opus-4-6",
		Prompt:               "do the thing",
		Steering:             runner.SteeringScheduled,
		MaxTurns:             60,
		MaxBudgetUSD:         5.0,
		Bare:                 true,
		StrictMCPConfig:      true,
		NoSessionPersistence: true,
		Tools: runner.ToolPolicy{
			Bash:    []string{"gh issue list*"},
			Builtin: []string{"Read", "Grep"},
		},
		MCP: runner.MCPPolicy{Servers: map[string]runner.MCPServerConfig{
			"hindsight": {Type: "http", URL: "http://localhost:8890/mcp/test/"},
		}},
		HomeDir: "/tmp/alfred-run-X",
	}
	args := BuildArgs(cfg, "/tmp/alfred-run-X/mcp.json")

	assert.Contains(t, args, "--bare")
	assert.Contains(t, args, "-p")
	assert.Contains(t, args, "do the thing")
	assert.Contains(t, args, "--model")
	assert.Contains(t, args, "claude-opus-4-6")
	assert.Contains(t, args, "--output-format")
	assert.Contains(t, args, "stream-json")
	assert.Contains(t, args, "--verbose")
	assert.Contains(t, args, "--strict-mcp-config")
	assert.Contains(t, args, "--mcp-config")
	assert.Contains(t, args, "/tmp/alfred-run-X/mcp.json")
	assert.Contains(t, args, "--permission-mode")
	assert.Contains(t, args, "dontAsk")
	assert.Contains(t, args, "--no-session-persistence")
	assert.Contains(t, args, "--max-turns")
	assert.Contains(t, args, "60")
	assert.Contains(t, args, "--max-budget-usd")
	assert.Contains(t, args, "5.00")
	assert.Contains(t, args, "mcp__hindsight__*")
}

func TestBuildArgs_FlagsOmittedWhenFalse(t *testing.T) {
	cfg := runner.RunConfig{
		Prompt:               "test",
		Steering:             runner.SteeringScheduled,
		Bare:                 false,
		StrictMCPConfig:      false,
		NoSessionPersistence: false,
	}
	args := BuildArgs(cfg, "")

	assert.NotContains(t, args, "--bare")
	assert.NotContains(t, args, "--no-session-persistence")
	assert.NotContains(t, args, "--strict-mcp-config")
}

func TestBuildArgs_MCPConfigWithoutStrict(t *testing.T) {
	cfg := runner.RunConfig{
		Prompt:          "test",
		Steering:        runner.SteeringScheduled,
		StrictMCPConfig: false,
	}
	args := BuildArgs(cfg, "/tmp/mcp.json")

	assert.Contains(t, args, "--mcp-config")
	assert.Contains(t, args, "/tmp/mcp.json")
	assert.NotContains(t, args, "--strict-mcp-config")
}

func TestBuildArgs_AllowedToolsTranslated(t *testing.T) {
	cfg := runner.RunConfig{
		Steering: runner.SteeringScheduled,
		Tools: runner.ToolPolicy{
			Bash:    []string{"gh issue list*", "github-issues comment*"},
			Builtin: []string{"Read", "Edit"},
		},
	}
	args := BuildArgs(cfg, "")

	idx := indexOf(args, "--allowedTools")
	if idx < 0 {
		t.Fatal("expected --allowedTools")
	}
	rest := args[idx+1:]
	assert.Contains(t, rest, "Bash(gh issue list*)")
	assert.Contains(t, rest, "Bash(github-issues comment*)")
	assert.Contains(t, rest, "Read")
	assert.Contains(t, rest, "Edit")
}

func TestBuildArgs_InteractiveSetsPromptTool(t *testing.T) {
	cfg := runner.RunConfig{
		Steering:       runner.SteeringInteractive,
		PermissionTool: "mcp__alfred-perm__permission_request",
	}
	args := BuildArgs(cfg, "/tmp/x/mcp.json")
	assert.Contains(t, args, "--permission-prompt-tool")
	assert.Contains(t, args, "mcp__alfred-perm__permission_request")
	idx := indexOf(args, "--permission-mode")
	if idx >= 0 {
		assert.NotEqual(t, "dontAsk", args[idx+1])
	}
}

func TestBuildArgs_GuidanceEnabledAddsInputStreamFlags(t *testing.T) {
	cfg := runner.RunConfig{
		Prompt:   "do work",
		Steering: runner.SteeringScheduled,
		Guidance: make(<-chan string),
	}
	args := BuildArgs(cfg, "")

	assert.Contains(t, args, "--input-format")
	idx := indexOf(args, "--input-format")
	assert.Greater(t, idx, -1)
	assert.Equal(t, "stream-json", args[idx+1])
	assert.Contains(t, args, "--replay-user-messages")
}

func TestBuildArgs_NoGuidance_NoInputStreamFlags(t *testing.T) {
	cfg := runner.RunConfig{
		Prompt:   "do work",
		Steering: runner.SteeringScheduled,
	}
	args := BuildArgs(cfg, "")

	assert.NotContains(t, args, "--input-format")
	assert.NotContains(t, args, "--replay-user-messages")
}

func indexOf(slice []string, target string) int {
	for i, s := range slice {
		if s == target {
			return i
		}
	}
	return -1
}
