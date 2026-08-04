package claude

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tphakala/alfred/internal/agent/runner"
)

func fakeBinaryPath(t *testing.T) string {
	abs, err := filepath.Abs("testdata/fake-claude.sh")
	require.NoError(t, err)
	return abs
}

func TestRunner_Run_EmitsExpectedEvents(t *testing.T) {
	r := New(Config{BinaryPath: fakeBinaryPath(t)})
	events := make(chan runner.Event, 16)

	res, err := r.Run(context.Background(), runner.RunConfig{
		Model:    "claude-test",
		Prompt:   "hi",
		Steering: runner.SteeringScheduled,
		HomeDir:  t.TempDir(),
	}, events)
	require.NoError(t, err)
	assert.Equal(t, 0, res.ExitCode)

	close(events)
	var kinds []runner.EventKind
	for ev := range events {
		kinds = append(kinds, ev.Kind)
	}
	assert.Equal(t, []runner.EventKind{
		runner.KindSessionStart,
		runner.KindTextDelta,
		runner.KindDone,
	}, kinds)
}

func TestRunner_Run_CancellationTerminatesSubprocess(t *testing.T) {
	slowFake := filepath.Join(t.TempDir(), "slow.sh")
	require.NoError(t, os.WriteFile(slowFake, []byte("#!/usr/bin/env bash\nsleep 30\n"), 0o755))

	r := New(Config{BinaryPath: slowFake})
	events := make(chan runner.Event, 4)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, err := r.Run(ctx, runner.RunConfig{
		Steering: runner.SteeringScheduled,
		HomeDir:  t.TempDir(),
	}, events)
	elapsed := time.Since(start)

	assert.True(t, errors.Is(err, context.Canceled), "expected context.Canceled, got %v", err)
	assert.Less(t, elapsed, 10*time.Second, "subprocess should be killed quickly, elapsed=%v", elapsed)
}

func TestRunner_Run_GuidanceUnblocksOnNormalExit(t *testing.T) {
	r := New(Config{BinaryPath: fakeBinaryPath(t)})
	events := make(chan runner.Event, 16)
	// guidance is never closed and never sent to while the fake subprocess
	// runs, the same shape as a live but quiet steering channel; streamGuidance
	// only returns on ctx cancel or channel close, neither of which happens on
	// its own once the subprocess exits (issue #98).
	guidance := make(chan string)

	done := make(chan struct{})
	var res runner.RunResult
	var runErr error
	go func() {
		defer close(done)
		res, runErr = r.Run(t.Context(), runner.RunConfig{
			Model:    "claude-test",
			Prompt:   "hi",
			Steering: runner.SteeringScheduled,
			HomeDir:  t.TempDir(),
			Guidance: guidance,
		}, events)
	}()

	select {
	case <-done:
		// ok
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after the subprocess exited with an idle guidance channel (issue #98)")
	}

	require.NoError(t, runErr)
	assert.Equal(t, 0, res.ExitCode)
}

func TestRunner_NameAndCapabilities(t *testing.T) {
	r := New(Config{BinaryPath: "/usr/bin/true"})
	assert.Equal(t, "claude", r.Name())
	caps := r.Capabilities()
	assert.True(t, caps.StructuredOutput)
	assert.True(t, caps.PerCallPermissionHook)
	assert.True(t, caps.BidirectionalStream)
}

func TestRunner_Run_MCPConfigCreatedAndCleaned(t *testing.T) {
	r := New(Config{BinaryPath: fakeBinaryPath(t)})
	events := make(chan runner.Event, 16)
	homeDir := t.TempDir()

	_, err := r.Run(context.Background(), runner.RunConfig{
		Model:    "claude-test",
		Prompt:   "hi",
		Steering: runner.SteeringScheduled,
		HomeDir:  homeDir,
		MCP: runner.MCPPolicy{Servers: map[string]runner.MCPServerConfig{
			"test": {Type: "http", URL: "http://localhost/"},
		}},
	}, events)
	require.NoError(t, err)

	mcpPath := filepath.Join(homeDir, "mcp.json")
	_, err = os.Stat(mcpPath)
	assert.True(t, os.IsNotExist(err), "mcp.json should be cleaned up after run")
}

func TestRunner_Run_EnvInjectedToSubprocess(t *testing.T) {
	envScript := filepath.Join(t.TempDir(), "env-claude.sh")
	require.NoError(t, os.WriteFile(envScript, []byte(`#!/usr/bin/env bash
set -euo pipefail
cat <<EOF
{"type":"system","subtype":"init","session_id":"sess-env","model":"claude-test"}
{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"bank=${HINDSIGHT_BANK:-unset}"}}}
{"type":"result","is_error":false,"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15},"total_cost_usd":0.001}
EOF
exit 0
`), 0o755))

	r := New(Config{BinaryPath: envScript})
	events := make(chan runner.Event, 16)

	_, err := r.Run(context.Background(), runner.RunConfig{
		Model:    "claude-test",
		Prompt:   "hi",
		Steering: runner.SteeringScheduled,
		HomeDir:  t.TempDir(),
		Env:      map[string]string{"HINDSIGHT_BANK": "my-bank"},
	}, events)
	require.NoError(t, err)
	close(events)

	var textDelta string
	for ev := range events {
		if ev.Kind == runner.KindTextDelta {
			textDelta += ev.Text
		}
	}
	assert.Contains(t, textDelta, "bank=my-bank",
		"env vars from RunConfig.Env must be visible to the subprocess")
}

func TestRunner_Run_NoMCPConfigWhenNoServers(t *testing.T) {
	r := New(Config{BinaryPath: fakeBinaryPath(t)})
	events := make(chan runner.Event, 16)
	homeDir := t.TempDir()

	_, err := r.Run(context.Background(), runner.RunConfig{
		Model:    "claude-test",
		Prompt:   "hi",
		Steering: runner.SteeringScheduled,
		HomeDir:  homeDir,
	}, events)
	require.NoError(t, err)

	mcpPath := filepath.Join(homeDir, "mcp.json")
	_, err = os.Stat(mcpPath)
	assert.True(t, os.IsNotExist(err), "no mcp.json should be created when no servers configured")
}
