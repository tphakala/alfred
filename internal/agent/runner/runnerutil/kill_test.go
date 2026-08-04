package runnerutil_test

import (
	"bufio"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tphakala/alfred/internal/agent/runner/runnerutil"
)

func TestKillGroup_NoOpOnInvalidPid(t *testing.T) {
	// A pid <= 0 must be a no-op so a process that never started is safe to
	// signal (and so a negative-pid kill can never target the caller's own
	// group by accident: syscall.Kill(-0, ...) == Kill(0, ...) signals our own
	// group; -(-1) == Kill(1, ...) would signal init).
	assert.NoError(t, runnerutil.KillGroup(0, syscall.SIGTERM))
	assert.NoError(t, runnerutil.KillGroup(-1, syscall.SIGKILL))
}

func TestKillWithGrace_ReapsProcessGroup(t *testing.T) {
	cmd := exec.Command("sleep", "60")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	require.NoError(t, cmd.Start())
	t.Cleanup(func() { _ = runnerutil.KillGroup(cmd.Process.Pid, syscall.SIGKILL) })

	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()

	// A plain sleep has no SIGTERM handler, so it dies in the first grace
	// window: reaped must be true and KillWithGrace must have drained waitDone
	// (so the reaping goroutine cannot leak).
	reaped := runnerutil.KillWithGrace(cmd.Process.Pid, waitDone)
	assert.True(t, reaped, "a plain sleep must be reaped by SIGTERM within a grace window")

	// Wait has returned (KillWithGrace received from waitDone), so the process
	// is gone: signalling the reaped pid errors.
	assert.Error(t, cmd.Process.Signal(syscall.Signal(0)), "the process should no longer exist")
}

func TestKillWithGrace_ReapsGroupDescendants(t *testing.T) {
	// The whole point of Setpgid + the negative-pid kill: a descendant in the
	// group dies too. The leader backgrounds a child sleep into its own group
	// and writes the child's PID out fd 3; a leader-only kill (+pid) would
	// orphan that child, so this test fails if KillGroup ever dropped the
	// negative-pid group semantics.
	pr, pw, err := os.Pipe()
	require.NoError(t, err)
	t.Cleanup(func() { _ = pr.Close() })

	cmd := exec.Command("sh", "-c", "sleep 60 & echo $! >&3; wait")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.ExtraFiles = []*os.File{pw} // becomes fd 3 in the child
	require.NoError(t, cmd.Start())
	_ = pw.Close() // drop the parent's write end so ReadAll sees EOF after the echo
	t.Cleanup(func() { _ = runnerutil.KillGroup(cmd.Process.Pid, syscall.SIGKILL) })

	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()

	// Read only the single PID line. Reading to EOF would block until the whole
	// group exits, because the backgrounded sleep also inherits fd 3 and holds
	// its write end open for its full lifetime.
	line, err := bufio.NewReader(pr).ReadString('\n')
	require.NoError(t, err)
	childPID, err := strconv.Atoi(strings.TrimSpace(line))
	require.NoError(t, err)
	require.Positive(t, childPID)

	require.True(t, runnerutil.KillWithGrace(cmd.Process.Pid, waitDone),
		"the group leader must be reaped by SIGTERM")

	// The backgrounded child was in the same group, so the group SIGTERM
	// reached it. It is reaped asynchronously (reparented to init), so poll
	// until signalling it returns ESRCH.
	assert.Eventually(t, func() bool {
		return errors.Is(syscall.Kill(childPID, 0), syscall.ESRCH)
	}, 3*time.Second, 10*time.Millisecond,
		"the group's backgrounded child must be killed with the group, not orphaned")
}
