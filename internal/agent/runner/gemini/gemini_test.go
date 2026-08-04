package gemini_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tphakala/alfred/internal/agent/runner"
	"github.com/tphakala/alfred/internal/agent/runner/gemini"
)

// fakeGemini is a tiny shell script that prints a known stream-json payload
// and exits 0. The test writes it into a temp dir and points the runner at it.
const fakeGeminiScript = `#!/bin/sh
cat <<'EOF'
{"type":"system","subtype":"start","session_id":"s-1","model":"gemini-test"}
{"type":"text_delta","text":"ok"}
{"type":"result","is_error":false,"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2},"total_cost_usd":0}
EOF
`

func TestRunner_Run_HappyPath(t *testing.T) {
	tmp := t.TempDir()
	binPath := filepath.Join(tmp, "gemini")
	require.NoError(t, os.WriteFile(binPath, []byte(fakeGeminiScript), 0o755))

	t.Setenv("GEMINI_API_KEY", "should-be-scrubbed")
	t.Setenv("GOOGLE_API_KEY", "should-be-scrubbed")

	r := gemini.New(gemini.Config{BinaryPath: binPath})

	events := make(chan runner.Event, 8)
	res, err := r.Run(context.Background(), runner.RunConfig{
		Prompt:   "hi",
		Model:    "gemini-test",
		Steering: runner.SteeringScheduled,
	}, events)
	close(events)
	require.NoError(t, err)
	assert.Equal(t, 0, res.ExitCode)

	var kinds []runner.EventKind
	for ev := range events {
		kinds = append(kinds, ev.Kind)
	}
	assert.Contains(t, kinds, runner.KindSessionStart)
	assert.Contains(t, kinds, runner.KindTextDelta)
	assert.Contains(t, kinds, runner.KindDone)
}

func TestRunner_Run_ScrubsAuthEnv(t *testing.T) {
	tmp := t.TempDir()
	binPath := filepath.Join(tmp, "gemini")
	// Print the environment so we can assert on it.
	script := `#!/bin/sh
env > "` + filepath.Join(tmp, "captured.env") + `"
cat <<'EOF'
{"type":"result","is_error":false}
EOF
`
	require.NoError(t, os.WriteFile(binPath, []byte(script), 0o755))

	t.Setenv("GEMINI_API_KEY", "leaked")
	t.Setenv("GOOGLE_API_KEY", "leaked")

	r := gemini.New(gemini.Config{BinaryPath: binPath})
	events := make(chan runner.Event, 8)
	_, err := r.Run(context.Background(), runner.RunConfig{Prompt: "hi"}, events)
	close(events)
	require.NoError(t, err)

	got, err := os.ReadFile(filepath.Join(tmp, "captured.env"))
	require.NoError(t, err)
	assert.NotContains(t, string(got), "GEMINI_API_KEY=leaked")
	assert.NotContains(t, string(got), "GOOGLE_API_KEY=leaked")
}
