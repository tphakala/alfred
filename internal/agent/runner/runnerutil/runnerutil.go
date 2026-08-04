// Package runnerutil contains low-level helpers shared by Alfred's external
// agent runner adapters (claude, gemini, copilot). The package is deliberately
// narrow: stateless subprocess lifecycle primitives (bounded stderr capture,
// kill-grace constants, error-message truncation), the shared streaming
// lifecycle skeleton (RunPipeStreaming), and per-run MCP config authoring.
// New helpers should fit one of those buckets or get their own package.
package runnerutil

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/tphakala/alfred/internal/agent/runner"
)

// StderrReadLimit bounds how much subprocess stderr is buffered for
// diagnostics. Past this, the pipe is drained but the captured suffix is
// dropped so a runaway child cannot OOM the worker through stderr alone.
const StderrReadLimit = 1 * 1024 * 1024 // 1 MiB

// StderrTruncationMarker is appended to the captured stderr prefix when
// bytes were discarded past StderrReadLimit.
const StderrTruncationMarker = "\n[stderr truncated at 1MiB]"

// KillGrace bounds how long we wait between SIGTERM and SIGKILL (and again
// between SIGKILL and abandoning the wait) when context cancellation forces
// a runner subprocess down. The second grace window matters when a
// descendant escaped the process group (e.g. via setsid) and still holds
// stdout/stderr pipes open: without it cmd.Wait would block indefinitely
// waiting for those pipes to close. Callers in the abandon path should also
// Close the pipes from their side so the reader goroutines see EOF; see
// RunPipeStreaming's cancel branch for the pattern.
const KillGrace = 5 * time.Second

// KillGroup sends sig to the entire process group led by pid (a negative-pid
// kill). A pid <= 0 is a no-op, so callers need not special-case a process
// that never started. Subprocesses spawned with SysProcAttr.Setpgid lead their
// own group, so this signals the whole subtree, not just the leader.
func KillGroup(pid int, sig syscall.Signal) error {
	if pid <= 0 {
		return nil
	}
	return syscall.Kill(-pid, sig)
}

// KillWithGrace tears down the process group led by pid after a context
// cancellation forces a subprocess down. It SIGTERMs the group, waits up to
// KillGrace for waitDone; if the process has not exited it SIGKILLs and waits
// up to KillGrace again. It reports whether the process was reaped (waitDone
// fired within a grace window).
//
// When it returns false both windows expired: a descendant likely escaped the
// process group (e.g. via setsid) and still holds the subprocess's
// stdout/stderr, so a caller that owns those pipes should Close them to unblock
// its reader goroutines. waitDone is the buffered channel a
// `waitDone <- cmd.Wait()` goroutine sends the Wait result on; it is only
// received from here, never closed, so the send always succeeds and the
// goroutine cannot leak on the reaped path.
func KillWithGrace(pid int, waitDone <-chan error) bool {
	_ = KillGroup(pid, syscall.SIGTERM)
	select {
	case <-waitDone:
		return true
	case <-time.After(KillGrace):
	}
	_ = KillGroup(pid, syscall.SIGKILL)
	select {
	case <-waitDone:
		return true
	case <-time.After(KillGrace):
		return false
	}
}

// errStderrTailLimit bounds how many bytes of subprocess stderr we embed
// into a returned error. Long stderr can contain bearer tokens, repo paths,
// and OAuth fragments that should not flow into Temporal workflow history or
// Sentry payloads verbatim; keeping only the tail balances operator
// debuggability with privacy.
const errStderrTailLimit = 256

// ReadBoundedStream reads up to limit bytes from r into a string. If more
// data is available it keeps draining r (so the subprocess writer does not
// block on a full pipe) but discards the excess, returning the captured
// prefix with StderrTruncationMarker appended. When the stream produces
// exactly limit bytes the marker is NOT appended: the limit was met
// exactly, nothing was discarded.
//
// A limit <= 0 is treated as zero: no bytes are captured and r is drained
// to EOF. Callers should pass StderrReadLimit.
func ReadBoundedStream(r io.Reader, limit int) string {
	if limit < 0 {
		limit = 0
	}
	limited := &io.LimitedReader{R: r, N: int64(limit)}
	var sb strings.Builder
	_, _ = io.Copy(&sb, limited)
	if limited.N > 0 {
		return sb.String()
	}
	discarded, _ := io.Copy(io.Discard, r)
	if discarded == 0 {
		return sb.String()
	}
	return sb.String() + StderrTruncationMarker
}

// TruncateForError returns a tail-truncated form of s suitable for embedding
// in a returned error. Strings longer than errStderrTailLimit are replaced
// with the last errStderrTailLimit bytes prefixed by "...". The tail is
// preferred over the head because subprocess error messages typically appear
// near the end of stderr.
//
// Tail truncation reduces but does not eliminate the risk of leaking secrets
// that appear at the tail of stderr; callers handling stderr that is known
// to contain credentials should scrub before passing it here.
func TruncateForError(s string) string {
	if len(s) <= errStderrTailLimit {
		return s
	}
	return "..." + s[len(s)-errStderrTailLimit:]
}

// WriteMCPConfig writes a per-run MCP config file to dir/mcp.json with the
// {"mcpServers": {...}} wrapper shared by Claude Code and the GitHub Copilot
// CLI. Returns the absolute path to the written file. The file is created
// with 0o600 permissions and its parent directory with 0o700.
func WriteMCPConfig(dir string, servers map[string]runner.MCPServerConfig) (string, error) {
	wrapper := struct {
		MCPServers map[string]runner.MCPServerConfig `json:"mcpServers"`
	}{MCPServers: servers}

	data, err := json.Marshal(wrapper)
	if err != nil {
		return "", fmt.Errorf("marshal mcp config: %w", err)
	}

	absDir, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("resolve run dir: %w", err)
	}
	resolved := filepath.Join(absDir, "mcp.json")
	rel, relErr := filepath.Rel(absDir, resolved)
	if relErr != nil {
		return "", fmt.Errorf("compute relative mcp config path: %w", relErr)
	}
	if strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("mcp config path %q escapes run dir %q", resolved, absDir)
	}

	if err := os.MkdirAll(absDir, 0o700); err != nil {
		return "", fmt.Errorf("create mcp config dir: %w", err)
	}
	if err := os.WriteFile(resolved, data, 0o600); err != nil {
		return "", fmt.Errorf("write mcp config file: %w", err)
	}
	return resolved, nil
}

// PrepareMCPConfig writes a per-run MCP config file when servers is
// non-empty. When homeDir is empty a fresh temp directory is created so the
// caller does not need to manage one. The returned cleanup func releases the
// temp directory (or removes the single mcp.json when homeDir was provided);
// it is always non-nil so callers can `defer cleanup()` unconditionally.
//
// When servers is empty, PrepareMCPConfig is a no-op: an empty path and a
// no-op cleanup are returned.
func PrepareMCPConfig(homeDir string, servers map[string]runner.MCPServerConfig) (string, func(), error) {
	noop := func() {}
	if len(servers) == 0 {
		return "", noop, nil
	}
	dir := homeDir
	createdDir := false
	if dir == "" {
		var err error
		dir, err = os.MkdirTemp("", "alfred-run-*")
		if err != nil {
			return "", noop, fmt.Errorf("create run dir: %w", err)
		}
		createdDir = true
	}
	mcpPath, err := WriteMCPConfig(dir, servers)
	if err != nil {
		if createdDir {
			_ = os.RemoveAll(dir)
		}
		return "", noop, fmt.Errorf("write mcp config: %w", err)
	}
	if createdDir {
		dirPath := dir
		return mcpPath, func() { _ = os.RemoveAll(dirPath) }, nil
	}
	path := mcpPath
	return mcpPath, func() {
		if rmErr := os.Remove(path); rmErr != nil && !os.IsNotExist(rmErr) {
			log.Printf("warn: failed to remove mcp config %s: %v", path, rmErr)
		}
	}, nil
}
