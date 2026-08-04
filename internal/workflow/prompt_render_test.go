package workflow

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tphakala/alfred/internal/agentcfg"
)

func TestRenderPrompt_BaseOnly(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "base.md"), []byte("hello"), 0o644))

	out, err := RenderPrompt(dir, agentcfg.Prompt{Base: "base.md"}, nil)
	require.NoError(t, err)
	assert.Equal(t, "hello", out)
}

func TestRenderPrompt_HeaderPlusIncludes(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "base.md"), []byte("BASE"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "voice.md"), []byte("VOICE"), 0o644))

	out, err := RenderPrompt(dir, agentcfg.Prompt{
		Base:     "base.md",
		Includes: []string{"voice.md"},
		Header:   "HEAD",
	}, nil)
	require.NoError(t, err)
	assert.Equal(t, "HEAD\nVOICE\nBASE", out)
}

func TestRenderPrompt_PathTraversalBlocked(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "base.md"), []byte("ok"), 0o644))

	_, err := RenderPrompt(dir, agentcfg.Prompt{Base: "../../etc/passwd"}, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "escapes prompt directory")

	_, err = RenderPrompt(dir, agentcfg.Prompt{
		Base:     "base.md",
		Includes: []string{"../../../etc/shadow"},
	}, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "escapes prompt directory")
}

func TestRenderPrompt_HeaderTemplateVars(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "base.md"), []byte("BASE"), 0o644))

	out, err := RenderPrompt(dir, agentcfg.Prompt{
		Base:   "base.md",
		Header: "Candidates: {{ .prefilter.candidates }}",
	}, map[string]any{
		"prefilter": map[string]any{"candidates": "1,2,3"},
	})
	require.NoError(t, err)
	assert.Equal(t, "Candidates: 1,2,3\nBASE", out)
}
