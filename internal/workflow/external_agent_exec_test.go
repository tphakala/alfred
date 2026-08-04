package workflow

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCappedBuffer_BelowLimit(t *testing.T) {
	b := newCappedBuffer(100)
	n, err := b.Write([]byte("hello"))
	require.NoError(t, err)
	assert.Equal(t, 5, n)
	assert.Equal(t, "hello", string(b.Bytes()))
}

func TestCappedBuffer_AtLimit(t *testing.T) {
	b := newCappedBuffer(5)
	n, err := b.Write([]byte("hello"))
	require.NoError(t, err)
	assert.Equal(t, 5, n)
	assert.Equal(t, "hello", string(b.Bytes()))
}

func TestCappedBuffer_AboveLimitTruncatesWithMarker(t *testing.T) {
	b := newCappedBuffer(5)
	n, err := b.Write([]byte("hello world"))
	require.NoError(t, err)
	assert.Equal(t, 11, n, "must report all input bytes consumed even when truncated")

	out := string(b.Bytes())
	assert.True(t, strings.HasPrefix(out, "hello"), "first 5 bytes must be preserved, got %q", out)
	assert.Contains(t, out, "output truncated", "truncation marker must be appended")
}

func TestCappedBuffer_SubsequentWritesDroppedAfterTruncation(t *testing.T) {
	b := newCappedBuffer(3)
	_, _ = b.Write([]byte("abcde"))
	_, _ = b.Write([]byte("fghij"))
	out := string(b.Bytes())
	assert.True(t, strings.HasPrefix(out, "abc"))
	assert.NotContains(t, out, "de", "data beyond limit must be dropped")
	assert.NotContains(t, out, "fgh", "writes after truncation must be discarded")
}

func TestCappedBuffer_ConcurrentWrites(t *testing.T) {
	b := newCappedBuffer(10000)
	var wg sync.WaitGroup
	for range 100 {
		wg.Go(func() {
			_, _ = b.Write([]byte("0123456789"))
		})
	}
	wg.Wait()
	out := b.Bytes()
	assert.LessOrEqual(t, len(out), 10000+len(subprocessTruncationMarker),
		"concurrent writes must respect the cap")
}

func TestRunBashCommand_CapsOutputAt1MB(t *testing.T) {
	// Print 2 MB of "x". Output must be capped to subprocessOutputLimit with marker.
	stdout, _, err := runBashCommand(t.Context(), "head -c 2097152 /dev/zero | tr '\\0' 'x'", true)
	require.NoError(t, err)

	assert.LessOrEqual(t, len(stdout), subprocessOutputLimit+len(subprocessTruncationMarker),
		"combined output must be capped")
	assert.True(t, bytes.HasSuffix(stdout, subprocessTruncationMarker),
		"truncation marker must be present, got tail %q", string(stdout[len(stdout)-50:]))
}

func TestRunBashCommand_SeparateStdoutStderr(t *testing.T) {
	stdout, stderr, err := runBashCommand(t.Context(),
		`echo to-stdout; echo to-stderr >&2`, false)
	require.NoError(t, err)
	assert.Equal(t, "to-stdout\n", string(stdout))
	assert.Equal(t, "to-stderr\n", string(stderr))
}

func TestRunBashCommand_CombinedMergesStreams(t *testing.T) {
	stdout, stderr, err := runBashCommand(t.Context(),
		`echo to-stdout; echo to-stderr >&2`, true)
	require.NoError(t, err)
	assert.Nil(t, stderr, "combined mode returns nothing for the stderr slot")
	merged := string(stdout)
	assert.Contains(t, merged, "to-stdout")
	assert.Contains(t, merged, "to-stderr")
}

func TestRunBashCommand_NonZeroExitReturnsError(t *testing.T) {
	_, _, err := runBashCommand(t.Context(), "exit 7", true)
	require.Error(t, err)
	var exitErr *exec.ExitError
	require.ErrorAs(t, err, &exitErr)
	assert.Equal(t, 7, exitErr.ExitCode())
}

// TestRunBashCommand_CancelKillsProcessGroup verifies #15: when ctx is
// cancelled, the entire process group (not just bash) is killed. The script
// backgrounds a sleep child and writes its PID to a tempfile; after cancel we
// assert the child PID is no longer running.
func TestRunBashCommand_CancelKillsProcessGroup(t *testing.T) {
	pidFile := t.TempDir() + "/child.pid"
	// Background a sleep, write its PID to a file, then wait. On bash death
	// the orphaned sleep would normally survive; with process-group SIGTERM
	// it dies too.
	script := `sleep 30 &
echo -n $! > ` + pidFile + `
wait`

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, _, err := runBashCommand(ctx, script, true)
		done <- err
	}()

	// Wait for the child PID file to appear (with a generous timeout).
	deadline := time.Now().Add(3 * time.Second)
	var pidStr []byte
	for time.Now().Before(deadline) {
		b, readErr := os.ReadFile(pidFile)
		if readErr == nil && len(b) > 0 {
			pidStr = b
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	require.NotEmpty(t, pidStr, "child sleep PID never written to %s", pidFile)

	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("runBashCommand did not return within 10s of cancel")
	}

	// Verify the child sleep process is gone. syscall.Kill(pid, 0) probes
	// liveness without sending a signal: returns nil if alive, ESRCH if dead.
	pid := 0
	for _, c := range pidStr {
		if c >= '0' && c <= '9' {
			pid = pid*10 + int(c-'0')
		}
	}
	require.Positive(t, pid)

	// Give the kernel a moment to reap the process group.
	time.Sleep(200 * time.Millisecond)

	err := syscall.Kill(pid, 0)
	assert.Equal(t, syscall.ESRCH, err,
		"child sleep PID %d still alive after cancel; process group cleanup failed", pid)
}

func TestRunBashCommand_RespectsContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, _, err := runBashCommand(ctx, "sleep 30", true)
		done <- err
	}()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(10 * time.Second):
		t.Fatal("runBashCommand did not return within 10s of cancel")
	}
}

// TestRunArgvCommand_NoShellLiteralArgv proves the security-critical
// invariant runArgvCommand exists for: argv elements are passed straight to
// exec.Command, never through a shell, so shell metacharacters in an element
// are never interpreted.
func TestRunArgvCommand_NoShellLiteralArgv(t *testing.T) {
	dangerous := "$(echo pwned); rm -rf /tmp/nonexistent; `echo also-pwned`"
	stdout, _, exitCode, err := runArgvCommand(t.Context(), []string{"/bin/echo", dangerous}, nil)
	require.NoError(t, err)
	assert.Equal(t, 0, exitCode)
	assert.Equal(t, dangerous+"\n", string(stdout), "argv element must be echoed back literally, not shell-expanded")
}

func TestRunArgvCommand_NonZeroExitCapturedNoError(t *testing.T) {
	_, _, exitCode, err := runArgvCommand(t.Context(), []string{"/bin/false"}, nil)
	require.NoError(t, err, "a non-zero exit is not an activity error")
	assert.Equal(t, 1, exitCode)
}

func TestRunArgvCommand_StartFailureReturnsError(t *testing.T) {
	_, _, exitCode, err := runArgvCommand(t.Context(), []string{"/nonexistent/binary-xyz"}, nil)
	require.Error(t, err)
	assert.Equal(t, 0, exitCode)
}

func TestRunArgvCommand_EmptyArgvReturnsError(t *testing.T) {
	_, _, _, err := runArgvCommand(t.Context(), nil, nil)
	require.Error(t, err)
}

func TestRunArgvCommand_CustomEnv(t *testing.T) {
	stdout, _, exitCode, err := runArgvCommand(t.Context(),
		[]string{"/usr/bin/printenv", "FOO"}, []string{"FOO=bar"})
	require.NoError(t, err)
	assert.Equal(t, 0, exitCode)
	assert.Equal(t, "bar\n", string(stdout))
}

func TestRunArgvCommand_SeparateStdoutStderr(t *testing.T) {
	stdout, stderr, exitCode, err := runArgvCommand(t.Context(),
		[]string{"/bin/sh", "-c", "echo to-stdout; echo to-stderr >&2"}, nil)
	require.NoError(t, err)
	assert.Equal(t, 0, exitCode)
	assert.Equal(t, "to-stdout\n", string(stdout))
	assert.Equal(t, "to-stderr\n", string(stderr))
}

func TestRunArgvCommand_CapsOutputAt1MB(t *testing.T) {
	stdout, _, exitCode, err := runArgvCommand(t.Context(),
		[]string{"head", "-c", "2097152", "/dev/zero"}, nil)
	require.NoError(t, err)
	assert.Equal(t, 0, exitCode)
	assert.LessOrEqual(t, len(stdout), subprocessOutputLimit+len(subprocessTruncationMarker))
	assert.True(t, bytes.HasSuffix(stdout, subprocessTruncationMarker))
}

func TestRunArgvCommand_RespectsContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct {
		exitCode int
		err      error
	}, 1)
	go func() {
		_, _, exitCode, err := runArgvCommand(ctx, []string{"sleep", "30"}, nil)
		done <- struct {
			exitCode int
			err      error
		}{exitCode, err}
	}()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case res := <-done:
		require.ErrorIs(t, res.err, context.Canceled)
		assert.Equal(t, -1, res.exitCode)
	case <-time.After(10 * time.Second):
		t.Fatal("runArgvCommand did not return within 10s of cancel")
	}
}
