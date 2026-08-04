package workflow

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"sync"
	"syscall"

	"github.com/tphakala/alfred/internal/agent/runner/runnerutil"
)

// subprocessOutputLimit bounds memory used to capture a subprocess's combined
// or stream-specific output. Past this size, further writes are discarded and a
// truncation marker is appended. Picked to be generous for normal command
// output without risking worker OOM on runaways.
const subprocessOutputLimit = 1 * 1024 * 1024 // 1 MiB

var subprocessTruncationMarker = []byte("\n[output truncated at 1MiB]")

// cappedBuffer is a thread-safe io.Writer with a hard limit on stored bytes.
// Writes beyond the limit are accepted (no short writes) but the excess is
// dropped; Bytes() appends a marker if any data was discarded. Used to keep
// subprocess output capture bounded regardless of how much the subprocess
// writes.
type cappedBuffer struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func newCappedBuffer(limit int) *cappedBuffer {
	return &cappedBuffer{limit: limit}
}

// Write returns len(p), nil even when bytes are dropped. This matches the
// expectation of the exec package's pipe-drain goroutines, which would
// otherwise stop reading on a short write and back-pressure the subprocess.
func (c *cappedBuffer) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.truncated {
		return len(p), nil
	}
	remaining := c.limit - c.buf.Len()
	if remaining <= 0 {
		c.truncated = true
		return len(p), nil
	}
	if len(p) > remaining {
		c.buf.Write(p[:remaining])
		c.truncated = true
		return len(p), nil
	}
	return c.buf.Write(p)
}

func (c *cappedBuffer) Bytes() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.truncated {
		return slices.Clone(c.buf.Bytes())
	}
	out := make([]byte, 0, c.buf.Len()+len(subprocessTruncationMarker))
	out = append(out, c.buf.Bytes()...)
	out = append(out, subprocessTruncationMarker...)
	return out
}

// runBashCommand executes "bash -c script" with bounded output capture and
// process-group isolation:
//
//   - Setpgid: true so the bash process and its children share a group ID
//     equal to the bash PID. This lets us SIGTERM/SIGKILL the whole tree.
//   - Output capped at subprocessOutputLimit per stream (or shared, if
//     combined is true), so a runaway subprocess cannot OOM the worker.
//   - On context cancellation: teardown is delegated to runnerutil.KillWithGrace
//     (SIGTERM the group, grace, SIGKILL, grace). Mirrors the CLI runners' flow.
//
// When combined is true, the merged stream is returned in stdout and stderr
// is nil. When combined is false, stdout and stderr are captured separately.
// The returned err is the cmd.Wait error (including *exec.ExitError for
// non-zero exits), or ctx.Err() when cancellation forced the teardown. A
// command that exits but leaves a grandchild holding stdout/stderr open past
// WaitDelay returns exec.ErrWaitDelay (a bounded failure, not an unbounded
// hang).
func runBashCommand(ctx context.Context, script string, combined bool) (stdout, stderr []byte, err error) {
	// Deliberately use exec.Command, not exec.CommandContext: cancellation is
	// handled in the select below so we can SIGTERM/SIGKILL the entire
	// process group, not just the bash leader. CommandContext's default
	// teardown sends SIGKILL to the immediate process only, which would
	// orphan any children spawned by the bash script.
	//nolint:noctx // ctx handled manually for process-group teardown
	cmd := exec.Command("bash", "-c", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// Bound cmd.Wait so a daemonized grandchild that inherits stdout/stderr
	// cannot hang the reaping goroutine (or the whole call in the no-cancel
	// path) forever: after the process exits, os/exec waits up to WaitDelay for
	// the output-copy goroutines, then force-closes the pipes and returns.
	// Sized at twice KillGrace so this bound always exceeds the full
	// KillWithGrace teardown (SIGTERM grace + SIGKILL grace): that keeps the
	// group SIGKILL guaranteed-sent before os/exec abandons the wait, so a
	// same-group grandchild that traps SIGTERM but holds the pipes open is
	// still force-killed rather than left running.
	cmd.WaitDelay = 2 * runnerutil.KillGrace //nolint:mnd // WaitDelay is two kill-grace windows (see comment above)

	var stdoutBuf, stderrBuf *cappedBuffer
	if combined {
		shared := newCappedBuffer(subprocessOutputLimit)
		cmd.Stdout = shared
		cmd.Stderr = shared
		stdoutBuf = shared
	} else {
		stdoutBuf = newCappedBuffer(subprocessOutputLimit)
		stderrBuf = newCappedBuffer(subprocessOutputLimit)
		cmd.Stdout = stdoutBuf
		cmd.Stderr = stderrBuf
	}

	if startErr := cmd.Start(); startErr != nil {
		return nil, nil, startErr
	}

	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()

	select {
	case <-ctx.Done():
		runnerutil.KillWithGrace(cmd.Process.Pid, waitDone)
		err = ctx.Err()
	case err = <-waitDone:
	}

	stdout = stdoutBuf.Bytes()
	if !combined {
		stderr = stderrBuf.Bytes()
	}
	return stdout, stderr, err
}

// runArgvCommand executes argv[0] with argv[1:] directly, never through a
// shell, with bounded output capture and process-group isolation identical
// to runBashCommand. It exists for RunDeclaredCommand: a deployment-declared
// command tool's arguments must never be interpolated through a shell, so
// this passes argv elements straight to exec.Command, which never invokes a
// shell to parse them.
//
// env replaces the child process's environment wholesale; pass nil to
// inherit exec.Command's default (the current process's environment).
//
// Unlike runBashCommand, a non-zero exit is reported through exitCode with a
// nil error, not through the returned error: RunDeclaredCommand needs to
// hand a non-zero exit back to the supervisor LLM as an ordinary tool
// result, not fail the Temporal activity over it. err is non-nil only when
// the process could not be started (e.g. binary not found), ctx was cancelled
// before the process exited, or the process exited but a grandchild held
// stdout/stderr open past WaitDelay (exec.ErrWaitDelay); in the cancellation
// case exitCode is -1 (mirrors the ctx.Done() branch below: the process never
// produced its own exit code).
func runArgvCommand(ctx context.Context, argv, env []string) (stdout, stderr []byte, exitCode int, err error) {
	if len(argv) == 0 {
		return nil, nil, 0, fmt.Errorf("runArgvCommand: empty argv")
	}

	// Deliberately use exec.Command, not exec.CommandContext, for the same
	// reason as runBashCommand: cancellation is handled in the select below
	// so the whole process group (not just argv[0]) can be torn down.
	//nolint:noctx // ctx handled manually for process-group teardown, mirrors runBashCommand
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// Bound cmd.Wait against a daemonized grandchild holding stdout/stderr open;
	// see runBashCommand.
	cmd.WaitDelay = runnerutil.KillGrace

	stdoutBuf := newCappedBuffer(subprocessOutputLimit)
	stderrBuf := newCappedBuffer(subprocessOutputLimit)
	cmd.Stdout = stdoutBuf
	cmd.Stderr = stderrBuf

	if startErr := cmd.Start(); startErr != nil {
		return nil, nil, 0, startErr
	}

	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()

	select {
	case <-ctx.Done():
		runnerutil.KillWithGrace(cmd.Process.Pid, waitDone)
		return stdoutBuf.Bytes(), stderrBuf.Bytes(), -1, ctx.Err()

	case waitErr := <-waitDone:
		stdout = stdoutBuf.Bytes()
		stderr = stderrBuf.Bytes()
		if waitErr == nil {
			return stdout, stderr, 0, nil
		}
		if exitErr, ok := errors.AsType[*exec.ExitError](waitErr); ok {
			return stdout, stderr, exitErr.ExitCode(), nil
		}
		// The process started but Wait failed for a reason other than a
		// non-zero exit (e.g. an I/O error reaping it); that is a real
		// activity error.
		return stdout, stderr, 0, waitErr
	}
}
