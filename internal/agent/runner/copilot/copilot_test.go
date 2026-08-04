package copilot_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tphakala/alfred/internal/agent/runner"
	"github.com/tphakala/alfred/internal/agent/runner/copilot"
)

const fakeCopilotScript = `#!/bin/sh
env | grep -E '^(COPILOT_HOME|COPILOT_CLI|GH_TOKEN)' > "${CAPTURE_FILE}"
cat <<'EOF'
{"type":"session_start","session_id":"s-1","model":"copilot-test"}
{"type":"complete","is_error":false,"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}
EOF
`

func TestRunner_Run_SetsEphemeralEnv(t *testing.T) {
	tmp := t.TempDir()
	binPath := filepath.Join(tmp, "copilot")
	capture := filepath.Join(tmp, "captured.env")
	require.NoError(t, os.WriteFile(binPath, []byte(fakeCopilotScript), 0o755))
	t.Setenv("CAPTURE_FILE", capture)
	// Provide a GH_TOKEN so the runner doesn't try to shell out to gh.
	t.Setenv("GH_TOKEN", "test-token")

	r := copilot.New(copilot.Config{BinaryPath: binPath})

	events := make(chan runner.Event, 8)
	res, err := r.Run(context.Background(), runner.RunConfig{
		Prompt:   "hi",
		HomeDir:  tmp,
		Steering: runner.SteeringScheduled,
	}, events)
	close(events)
	require.NoError(t, err)
	assert.Equal(t, 0, res.ExitCode)

	got, err := os.ReadFile(capture)
	require.NoError(t, err)
	assert.Contains(t, string(got), "COPILOT_HOME=")
	assert.Contains(t, string(got), "COPILOT_CLI=1")
	assert.Contains(t, string(got), "GH_TOKEN=test-token")
}
