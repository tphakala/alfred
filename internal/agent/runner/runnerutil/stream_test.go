package runnerutil_test

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tphakala/alfred/internal/agent/runner"
	"github.com/tphakala/alfred/internal/agent/runner/runnerutil"
)

// writeFakeBinary writes an executable shell script and returns its path.
func writeFakeBinary(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-cli")
	require.NoError(t, os.WriteFile(path, []byte(script), 0o700))
	return path
}

// testSpec returns a StreamSpec over the fake binary with a JSON line parser:
// {"kind":"done","tokens":N,"cost":C} maps to a done event; a line with
// "malformed":true parses as a done event whose metrics extraction fails.
func testSpec(binPath string) runnerutil.StreamSpec {
	type fakeLine struct {
		Kind      string  `json:"kind"`
		Tokens    int     `json:"tokens"`
		Cost      float64 `json:"cost"`
		Malformed bool    `json:"malformed"`
	}
	parse := func(line []byte) (fakeLine, bool) {
		var fl fakeLine
		if err := json.Unmarshal(line, &fl); err != nil {
			return fakeLine{}, false
		}
		return fl, true
	}
	return runnerutil.StreamSpec{
		Name:       "fakecli",
		BinaryPath: binPath,
		ParseLine: func(line []byte) (runner.Event, bool) {
			fl, ok := parse(line)
			if !ok || fl.Kind == "" {
				return runner.Event{}, false
			}
			kind := runner.KindTextDelta
			if fl.Kind == "done" {
				kind = runner.KindDone
			}
			return runner.Event{Kind: kind}, true
		},
		ParseDoneMetrics: func(line []byte) (runner.TokenUsage, float64, bool) {
			fl, ok := parse(line)
			if !ok || fl.Malformed {
				return runner.TokenUsage{}, 0, false
			}
			return runner.TokenUsage{OutputTokens: fl.Tokens}, fl.Cost, true
		},
	}
}

// drainEvents consumes the events channel into a slice until the run returns.
func drainEvents(events <-chan runner.Event, done <-chan struct{}) []runner.Event {
	var out []runner.Event
	for {
		select {
		case ev := <-events:
			out = append(out, ev)
		case <-done:
			for {
				select {
				case ev := <-events:
					out = append(out, ev)
				default:
					return out
				}
			}
		}
	}
}

func TestRunPipeStreaming_FailedDoneMetricsKeepPreviousValues(t *testing.T) {
	// Two done lines: the first carries metrics, the second is a done event
	// whose metrics extraction fails (ok=false). The result must keep the
	// first line's values rather than zeroing them.
	bin := writeFakeBinary(t, `#!/bin/sh
echo '{"kind":"done","tokens":42,"cost":1.5}'
echo '{"kind":"done","malformed":true}'
`)
	spec := testSpec(bin)
	events := make(chan runner.Event, 8)
	done := make(chan struct{})
	var res runner.RunResult
	var runErr error
	go func() {
		defer close(done)
		res, runErr = runnerutil.RunPipeStreaming(t.Context(), &spec, events)
	}()
	got := drainEvents(events, done)

	require.NoError(t, runErr)
	assert.Equal(t, 0, res.ExitCode)
	assert.Equal(t, 42, res.TotalUsage.OutputTokens,
		"metrics from the parseable done line must survive the malformed one")
	assert.InEpsilon(t, 1.5, res.TotalCostUSD, 1e-9)
	require.Len(t, got, 2, "both done events must still be forwarded")
	assert.Equal(t, runner.KindDone, got[0].Kind)
	assert.Equal(t, runner.KindDone, got[1].Kind)
}

func TestRunPipeStreaming_StartErrorNamesRunner(t *testing.T) {
	spec := testSpec(filepath.Join(t.TempDir(), "does-not-exist"))
	events := make(chan runner.Event, 1)
	_, err := runnerutil.RunPipeStreaming(t.Context(), &spec, events)
	require.Error(t, err)
	assert.ErrorContains(t, err, "start fakecli")
}

func TestRunPipeStreaming_GuidanceUnblocksOnNormalExit(t *testing.T) {
	// The fake binary does not read stdin at all: it emits one done line and
	// exits immediately, mirroring a subprocess that finishes before the
	// caller's guidance stream has anything left to send.
	bin := writeFakeBinary(t, `#!/bin/sh
echo '{"kind":"done","tokens":1,"cost":0}'
exit 0
`)
	spec := testSpec(bin)
	// idle mimics an open guidance channel that is never sent to and never
	// closed while the subprocess is alive, the same shape as claude's
	// streamGuidance blocking on a live but quiet guidance chan.
	idle := make(chan string)
	spec.Guidance = func(ctx context.Context, _ io.Writer) {
		for {
			select {
			case <-ctx.Done():
				return
			case _, ok := <-idle:
				if !ok {
					return
				}
			}
		}
	}
	events := make(chan runner.Event, 8)
	done := make(chan struct{})
	var res runner.RunResult
	var runErr error
	go func() {
		defer close(done)
		res, runErr = runnerutil.RunPipeStreaming(t.Context(), &spec, events)
	}()

	select {
	case <-done:
		// ok
	case <-time.After(5 * time.Second):
		t.Fatal("RunPipeStreaming did not return after the subprocess exited with an idle guidance channel (issue #98)")
	}

	require.NoError(t, runErr)
	assert.Equal(t, 0, res.ExitCode)
}
