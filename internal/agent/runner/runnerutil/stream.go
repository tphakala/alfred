package runnerutil

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/tphakala/alfred/internal/agent/runner"
)

// Scanner buffer sizing for the stdout NDJSON stream: a generous initial
// buffer and a 4 MiB per-line ceiling (a single oversized line surfaces as a
// scanner error instead of silently truncating the stream).
const (
	scanBufInitial = 64 * 1024
	scanBufMax     = 4 * 1024 * 1024
)

// StreamSpec parameterizes RunPipeStreaming with the pieces that differ
// between the CLI runner adapters. Everything else about the subprocess
// lifecycle (process group, pipes, parse and stderr goroutines, teardown,
// result assembly) is shared.
type StreamSpec struct {
	// Name is the runner name used in error messages and warnings ("claude").
	Name string
	// BinaryPath is the executable to spawn.
	BinaryPath string
	// Args is the full argv (excluding the binary itself).
	Args []string
	// Env is the fully built subprocess environment, including any RunConfig
	// extras the adapter chose to merge in.
	Env []string
	// ParseLine converts one stdout line into an Event; ok=false skips the
	// line. Required.
	ParseLine func(line []byte) (runner.Event, bool)
	// ParseDoneMetrics extracts cumulative token usage and cost from a
	// KindDone event's raw line; ok=false keeps the previously captured
	// values. Required.
	ParseDoneMetrics func(line []byte) (usage runner.TokenUsage, costUSD float64, ok bool)
	// Guidance, when non-nil, runs on its own goroutine with the subprocess
	// stdin; the pipe is closed when it returns. Adapters without a stdin
	// protocol leave it nil and no stdin pipe is created. The callback MUST
	// return when ctx is cancelled or its input source closes; the run's
	// teardown waits for it on every exit path. RunPipeStreaming also cancels
	// that ctx as soon as the subprocess exits, so the callback does not need
	// to observe the exit itself.
	Guidance func(ctx context.Context, stdin io.Writer)
}

// consumeStdout scans the subprocess stdout line by line, forwards parsed
// events, and returns the token usage and cost captured from the last
// successfully parsed done event. It returns early when ctx is cancelled
// mid-send.
func consumeStdout(ctx context.Context, spec *StreamSpec, stdout io.Reader, events chan<- runner.Event) (usage runner.TokenUsage, costUSD float64) {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, scanBufInitial), scanBufMax)
	for scanner.Scan() {
		line := scanner.Bytes()
		ev, ok := spec.ParseLine(line)
		if !ok {
			continue
		}
		if ev.Kind == runner.KindDone {
			if u, c, mok := spec.ParseDoneMetrics(line); mok {
				usage = u
				costUSD = c
			}
		}
		select {
		case events <- ev:
		case <-ctx.Done():
			return usage, costUSD
		}
	}
	// Surface scanner errors (e.g. bufio.ErrTooLong if a single NDJSON line
	// exceeds the scan limit) so a silently truncated stream doesn't look
	// like a clean run.
	if scanErr := scanner.Err(); scanErr != nil {
		log.Printf("warn: %s stdout scanner: %v", spec.Name, scanErr)
	}
	return usage, costUSD
}

// RunPipeStreaming spawns the subprocess described by spec, parses each
// stdout line into a runner.Event, and forwards events until the subprocess
// exits or ctx is cancelled. It is the shared lifecycle skeleton behind the
// CLI runner adapters: a fix here applies to all of them.
func RunPipeStreaming(ctx context.Context, spec *StreamSpec, events chan<- runner.Event) (runner.RunResult, error) {
	// Fail fast before spawning: a nil parser would otherwise panic on the
	// parse goroutine after the child started, crashing the process and
	// orphaning the child in its own process group.
	if spec == nil || spec.ParseLine == nil || spec.ParseDoneMetrics == nil {
		return runner.RunResult{}, errors.New("runnerutil: StreamSpec.ParseLine and ParseDoneMetrics are required")
	}
	// ctx is deliberately handled manually (select + KillWithGrace) so the
	// whole process group is torn down, not just the direct child.
	cmd := exec.Command(spec.BinaryPath, spec.Args...) //nolint:noctx // ctx handled manually for process-group teardown
	cmd.Env = spec.Env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: 0}

	var stdinPipe io.WriteCloser
	if spec.Guidance != nil {
		var err error
		stdinPipe, err = cmd.StdinPipe()
		if err != nil {
			return runner.RunResult{}, fmt.Errorf("stdin pipe: %w", err)
		}
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return runner.RunResult{}, fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return runner.RunResult{}, fmt.Errorf("stderr pipe: %w", err)
	}

	start := time.Now()
	if err := cmd.Start(); err != nil {
		return runner.RunResult{}, fmt.Errorf("start %s: %w", spec.Name, err)
	}

	// Guidance runs on the run ctx but must also stop the instant the
	// subprocess exits: the guidance channel is closed by the caller's
	// deferred teardown, which runs only after Run returns, so without an
	// exit-driven cancel the stdinWG.Wait below would block until the
	// activity timeout (issue #98). guidanceCtx derives from ctx, so a parent
	// cancel still propagates; the waitErr branch cancels it explicitly on
	// normal exit.
	guidanceCtx, cancelGuidance := context.WithCancel(ctx)
	defer cancelGuidance()

	var stdinWG sync.WaitGroup
	if stdinPipe != nil {
		stdinWG.Go(func() {
			defer func() { _ = stdinPipe.Close() }()
			spec.Guidance(guidanceCtx, stdinPipe)
		})
	}

	parseDone := make(chan struct{})
	var totalUsage runner.TokenUsage
	var totalCost float64
	go func() {
		defer close(parseDone)
		totalUsage, totalCost = consumeStdout(ctx, spec, stdout, events)
	}()

	var stderrBuf string
	var stderrWG sync.WaitGroup
	stderrWG.Go(func() {
		stderrBuf = ReadBoundedStream(stderr, StderrReadLimit)
	})

	waitErr := make(chan error, 1)
	go func() { waitErr <- cmd.Wait() }()

	select {
	case <-ctx.Done():
		// SIGTERM then (after a grace) SIGKILL the whole process group. If both
		// grace windows expire the process is presumed dead but a descendant
		// that escaped the pgroup via setsid may still hold stdout/stderr open;
		// close them from our side to unblock the reader goroutines.
		if !KillWithGrace(cmd.Process.Pid, waitErr) {
			_ = stdout.Close()
			_ = stderr.Close()
		}
		<-parseDone
		stderrWG.Wait()
		stdinWG.Wait()
		return runner.RunResult{ExitCode: -1, DurationMS: time.Since(start).Milliseconds()}, ctx.Err()

	case err := <-waitErr:
		// subprocess exited: unblock the guidance goroutine before waiting on it (issue #98).
		cancelGuidance()
		<-parseDone
		stderrWG.Wait()
		stdinWG.Wait()
		exitCode := 0
		if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
			exitCode = exitErr.ExitCode()
		} else if err != nil {
			return runner.RunResult{}, fmt.Errorf("wait %s: %w (stderr tail: %s)", spec.Name, err, TruncateForError(stderrBuf))
		}
		return runner.RunResult{
			ExitCode:     exitCode,
			TotalUsage:   totalUsage,
			TotalCostUSD: totalCost,
			DurationMS:   time.Since(start).Milliseconds(),
		}, nil
	}
}
