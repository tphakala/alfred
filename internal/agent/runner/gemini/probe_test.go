package gemini_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tphakala/alfred/internal/agent/runner/gemini"
)

func TestProbe_StreamJSONSupported(t *testing.T) {
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "gemini")
	require.NoError(t, os.WriteFile(bin, []byte(`#!/bin/sh
echo "  --output-format stream-json"
`), 0o755))

	supported, err := gemini.ProbeStreamJSON(bin)
	require.NoError(t, err)
	assert.True(t, supported)
}

func TestProbe_StreamJSONUnsupported(t *testing.T) {
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "gemini")
	require.NoError(t, os.WriteFile(bin, []byte(`#!/bin/sh
echo "  --output-format text"
`), 0o755))

	supported, err := gemini.ProbeStreamJSON(bin)
	require.NoError(t, err)
	assert.False(t, supported)
}

func TestProbe_BinaryMissing(t *testing.T) {
	supported, err := gemini.ProbeStreamJSON("/nonexistent/gemini")
	require.Error(t, err)
	assert.False(t, supported)
}
