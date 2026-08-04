package runnerutil_test

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tphakala/alfred/internal/agent/runner"
	"github.com/tphakala/alfred/internal/agent/runner/runnerutil"
)

func TestReadBoundedStream_BelowLimit(t *testing.T) {
	got := runnerutil.ReadBoundedStream(strings.NewReader("hello"), 100)
	assert.Equal(t, "hello", got)
}

func TestReadBoundedStream_TruncatesAtLimit(t *testing.T) {
	huge := strings.Repeat("x", runnerutil.StderrReadLimit*2)
	got := runnerutil.ReadBoundedStream(strings.NewReader(huge), runnerutil.StderrReadLimit)

	assert.LessOrEqual(t, len(got), runnerutil.StderrReadLimit+len(runnerutil.StderrTruncationMarker),
		"output must be capped to StderrReadLimit plus marker")
	assert.True(t, strings.HasSuffix(got, runnerutil.StderrTruncationMarker),
		"truncation marker must be appended; got tail %q", got[len(got)-len(runnerutil.StderrTruncationMarker)-5:])
}

func TestReadBoundedStream_ExactlyAtLimitOmitsMarker(t *testing.T) {
	exact := strings.Repeat("z", runnerutil.StderrReadLimit)
	got := runnerutil.ReadBoundedStream(strings.NewReader(exact), runnerutil.StderrReadLimit)

	assert.Equal(t, exact, got,
		"when stream size matches the limit exactly, no bytes were discarded and the marker must be omitted")
}

func TestReadBoundedStream_DrainsAfterTruncation(t *testing.T) {
	r := &countingReader{src: strings.NewReader(strings.Repeat("y", runnerutil.StderrReadLimit*3))}
	got := runnerutil.ReadBoundedStream(r, runnerutil.StderrReadLimit)

	assert.Equal(t, runnerutil.StderrReadLimit*3, r.bytesRead,
		"helper must drain the underlying reader to EOF so subprocesses do not block")
	assert.True(t, strings.HasSuffix(got, runnerutil.StderrTruncationMarker))
}

func TestReadBoundedStream_NegativeLimitDrainsWithMarker(t *testing.T) {
	r := &countingReader{src: strings.NewReader("hello world")}
	got := runnerutil.ReadBoundedStream(r, -1)

	assert.Equal(t, runnerutil.StderrTruncationMarker, got,
		"negative limit captures nothing; the truncation marker still signals that bytes were discarded")
	assert.Equal(t, len("hello world"), r.bytesRead,
		"helper must still drain the underlying reader so writers do not block")
}

func TestTruncateForError_ShortPassesThrough(t *testing.T) {
	assert.Equal(t, "boom", runnerutil.TruncateForError("boom"))
	assert.Equal(t, "", runnerutil.TruncateForError(""))
}

func TestTruncateForError_LongTailOnly(t *testing.T) {
	long := strings.Repeat("a", 100) + "ERROR_AT_THE_TAIL"
	long += strings.Repeat("b", 500)
	got := runnerutil.TruncateForError(long)

	assert.True(t, strings.HasPrefix(got, "..."), "result must start with ellipsis prefix")
	assert.True(t, len(got) < len(long), "result must be shorter than input")
	assert.True(t, strings.HasSuffix(got, strings.Repeat("b", 256)),
		"result must keep the last 256 bytes of input")
}

func TestWriteMCPConfig_WritesValidJSON(t *testing.T) {
	dir := t.TempDir()
	servers := map[string]runner.MCPServerConfig{
		"test-server": {Type: "http", URL: "http://localhost:9999/mcp/test/"},
	}
	path, err := runnerutil.WriteMCPConfig(dir, servers)
	require.NoError(t, err)

	want, err := filepath.Abs(filepath.Join(dir, "mcp.json"))
	require.NoError(t, err)
	assert.Equal(t, want, path)

	data, err := os.ReadFile(path)
	require.NoError(t, err)

	var got struct {
		MCPServers map[string]runner.MCPServerConfig `json:"mcpServers"`
	}
	require.NoError(t, json.Unmarshal(data, &got))
	require.Contains(t, got.MCPServers, "test-server")
	assert.Equal(t, "http", got.MCPServers["test-server"].Type)
	assert.Equal(t, "http://localhost:9999/mcp/test/", got.MCPServers["test-server"].URL)
}

func TestWriteMCPConfig_FilePermissions(t *testing.T) {
	dir := t.TempDir()
	path, err := runnerutil.WriteMCPConfig(dir, map[string]runner.MCPServerConfig{
		"s": {URL: "http://localhost/"},
	})
	require.NoError(t, err)

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func TestWriteMCPConfig_StdioServer(t *testing.T) {
	dir := t.TempDir()
	servers := map[string]runner.MCPServerConfig{
		"stdio-server": {
			Command: "npx",
			Args:    []string{"-y", "@some/mcp-server"},
			Env:     map[string]string{"TOKEN": "abc123"},
		},
	}
	path, err := runnerutil.WriteMCPConfig(dir, servers)
	require.NoError(t, err)

	data, err := os.ReadFile(path)
	require.NoError(t, err)

	var got struct {
		MCPServers map[string]runner.MCPServerConfig `json:"mcpServers"`
	}
	require.NoError(t, json.Unmarshal(data, &got))
	s := got.MCPServers["stdio-server"]
	assert.Equal(t, "npx", s.Command)
	assert.Equal(t, []string{"-y", "@some/mcp-server"}, s.Args)
	assert.Equal(t, "abc123", s.Env["TOKEN"])
}

func TestWriteMCPConfig_RelativeDirResolvesToAbsolute(t *testing.T) {
	// When passed a relative dir, the helper must return an absolute path so
	// the caller can hand it to argv flags (--additional-mcp-config /
	// --mcp-config) which the subprocess interprets relative to its own cwd.
	wd := t.TempDir()
	t.Chdir(wd)

	relativeDir := "nested/run"
	path, err := runnerutil.WriteMCPConfig(relativeDir, map[string]runner.MCPServerConfig{
		"s": {URL: "http://localhost/"},
	})
	require.NoError(t, err)
	assert.True(t, filepath.IsAbs(path), "WriteMCPConfig must return an absolute path; got %q", path)
	assert.Equal(t, filepath.Join(wd, relativeDir, "mcp.json"), path)
}

func TestPrepareMCPConfig_NoServersIsNoop(t *testing.T) {
	path, cleanup, err := runnerutil.PrepareMCPConfig(t.TempDir(), nil)
	require.NoError(t, err)
	assert.Equal(t, "", path)
	require.NotNil(t, cleanup, "cleanup must be non-nil so callers can defer unconditionally")
	cleanup()
}

func TestPrepareMCPConfig_WithHomeDirRemovesFileOnCleanup(t *testing.T) {
	homeDir := t.TempDir()
	servers := map[string]runner.MCPServerConfig{"echo": {URL: "http://localhost/"}}
	path, cleanup, err := runnerutil.PrepareMCPConfig(homeDir, servers)
	require.NoError(t, err)
	require.NotEmpty(t, path)
	_, statErr := os.Stat(path)
	require.NoError(t, statErr, "mcp.json must exist between Prepare and cleanup")

	cleanup()
	_, statErr = os.Stat(path)
	assert.True(t, os.IsNotExist(statErr), "cleanup must remove mcp.json")

	// homeDir itself must NOT be removed when the caller owned it.
	_, statErr = os.Stat(homeDir)
	assert.NoError(t, statErr, "caller-owned homeDir must survive cleanup")
}

func TestPrepareMCPConfig_EmptyHomeDirCreatesAndRemovesTempDir(t *testing.T) {
	servers := map[string]runner.MCPServerConfig{"echo": {URL: "http://localhost/"}}
	path, cleanup, err := runnerutil.PrepareMCPConfig("", servers)
	require.NoError(t, err)
	require.NotEmpty(t, path)

	tempDir := filepath.Dir(path)
	_, statErr := os.Stat(tempDir)
	require.NoError(t, statErr, "Prepare must create the temp directory")
	require.True(t, strings.Contains(filepath.Base(tempDir), "alfred-run-"),
		"temp dir name must reflect the documented prefix; got %q", tempDir)

	cleanup()
	_, statErr = os.Stat(tempDir)
	assert.True(t, os.IsNotExist(statErr), "cleanup must remove the auto-created temp dir")
}

type countingReader struct {
	src       io.Reader
	bytesRead int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.src.Read(p)
	c.bytesRead += n
	return n, err
}
