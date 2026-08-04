package copilot

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tphakala/alfred/internal/agent/runner"
)

func TestRunner_Run_MCPConfigCreatedAndPassedToSubprocess(t *testing.T) {
	tmp := t.TempDir()
	binPath := filepath.Join(tmp, "copilot")
	require.NoError(t, copyFile("testdata/fake-copilot-mcp.sh", binPath, 0o755))

	homeDir := t.TempDir()
	argvCapture := filepath.Join(t.TempDir(), "argv.log")
	mcpCapture := filepath.Join(t.TempDir(), "mcp-seen.json")
	t.Setenv("ARGV_CAPTURE_FILE", argvCapture)
	t.Setenv("MCP_CAPTURE_FILE", mcpCapture)
	t.Setenv("GH_TOKEN", "test-token")

	r := New(Config{BinaryPath: binPath})
	events := make(chan runner.Event, 8)
	_, err := r.Run(context.Background(), runner.RunConfig{
		Prompt:   "hi",
		HomeDir:  homeDir,
		Steering: runner.SteeringScheduled,
		MCP: runner.MCPPolicy{Servers: map[string]runner.MCPServerConfig{
			"echo": {Type: "http", URL: "http://localhost:1234/mcp/echo/"},
		}},
	}, events)
	require.NoError(t, err)
	close(events)

	argv, err := os.ReadFile(argvCapture)
	require.NoError(t, err, "fake copilot should have captured argv")
	flags := strings.Split(strings.TrimSpace(string(argv)), "\n")
	assert.Contains(t, flags, "--additional-mcp-config", "BuildArgs must include --additional-mcp-config when MCP servers are configured")

	seen, err := os.ReadFile(mcpCapture)
	require.NoError(t, err, "fake copilot should have copied the mcp config file")
	var wrapper struct {
		MCPServers map[string]runner.MCPServerConfig `json:"mcpServers"`
	}
	require.NoError(t, json.Unmarshal(seen, &wrapper))
	require.Contains(t, wrapper.MCPServers, "echo")
	assert.Equal(t, "http://localhost:1234/mcp/echo/", wrapper.MCPServers["echo"].URL)

	mcpPath := filepath.Join(homeDir, "mcp.json")
	_, err = os.Stat(mcpPath)
	assert.True(t, os.IsNotExist(err), "mcp.json should be cleaned up after run")
}

// TestRunner_Run_MCPConfigInTempDirWhenHomeDirEmpty exercises the production
// code path. The workflow's runnerRunConfigFromPhase does not set HomeDir, so
// every real Copilot run with MCP servers goes through the temp-directory
// branch of runnerutil.PrepareMCPConfig.
func TestRunner_Run_MCPConfigInTempDirWhenHomeDirEmpty(t *testing.T) {
	tmp := t.TempDir()
	binPath := filepath.Join(tmp, "copilot")
	require.NoError(t, copyFile("testdata/fake-copilot-mcp.sh", binPath, 0o755))

	argvCapture := filepath.Join(t.TempDir(), "argv.log")
	mcpCapture := filepath.Join(t.TempDir(), "mcp-seen.json")
	t.Setenv("ARGV_CAPTURE_FILE", argvCapture)
	t.Setenv("MCP_CAPTURE_FILE", mcpCapture)
	t.Setenv("GH_TOKEN", "test-token")

	r := New(Config{BinaryPath: binPath})
	events := make(chan runner.Event, 8)
	_, err := r.Run(context.Background(), runner.RunConfig{
		Prompt:   "hi",
		Steering: runner.SteeringScheduled,
		MCP: runner.MCPPolicy{Servers: map[string]runner.MCPServerConfig{
			"echo": {Type: "http", URL: "http://localhost:1234/mcp/echo/"},
		}},
	}, events)
	require.NoError(t, err)
	close(events)

	argv, err := os.ReadFile(argvCapture)
	require.NoError(t, err)
	flags := strings.Split(strings.TrimSpace(string(argv)), "\n")
	assert.Contains(t, flags, "--additional-mcp-config")

	idx := -1
	for i, f := range flags {
		if f == "--additional-mcp-config" {
			idx = i
			break
		}
	}
	require.Greater(t, idx, -1)
	require.Less(t, idx+1, len(flags), "flag must be followed by a value")
	autoPath := flags[idx+1]
	autoDir := filepath.Dir(autoPath)
	assert.True(t, strings.HasPrefix(filepath.Base(autoDir), "alfred-run-"),
		"auto-created temp dir must use the documented prefix; got %q", autoDir)

	_, err = os.Stat(autoDir)
	assert.True(t, os.IsNotExist(err), "auto-created temp dir must be removed after run")
}

func TestRunner_Run_NoMCPConfigWhenNoServers(t *testing.T) {
	tmp := t.TempDir()
	binPath := filepath.Join(tmp, "copilot")
	require.NoError(t, copyFile("testdata/fake-copilot-mcp.sh", binPath, 0o755))

	homeDir := t.TempDir()
	argvCapture := filepath.Join(t.TempDir(), "argv.log")
	t.Setenv("ARGV_CAPTURE_FILE", argvCapture)
	t.Setenv("GH_TOKEN", "test-token")

	r := New(Config{BinaryPath: binPath})
	events := make(chan runner.Event, 8)
	_, err := r.Run(context.Background(), runner.RunConfig{
		Prompt:   "hi",
		HomeDir:  homeDir,
		Steering: runner.SteeringScheduled,
	}, events)
	require.NoError(t, err)
	close(events)

	argv, err := os.ReadFile(argvCapture)
	require.NoError(t, err)
	for _, line := range strings.Split(string(argv), "\n") {
		assert.NotEqual(t, "--additional-mcp-config", line, "MCP flag must be omitted when no servers are configured")
	}

	mcpPath := filepath.Join(homeDir, "mcp.json")
	_, err = os.Stat(mcpPath)
	assert.True(t, os.IsNotExist(err), "no mcp.json should be created when no servers are configured")
}

func copyFile(src, dst string, perm os.FileMode) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, perm)
}
