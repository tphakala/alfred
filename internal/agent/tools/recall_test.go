package tools

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tphakala/alfred/internal/memory"
)

// testQuery is the recurring query argument used across recall tests.
const testQuery = "query"

// fakeMemory satisfies memory.MemoryClient for tests.
type fakeMemory struct {
	recallResp *memory.RecallResponse
	recallErr  error
	gotQuery   string
}

func (f *fakeMemory) Retain(_ context.Context, _ memory.RetainRequest) error { return nil } //nolint:gocritic // hugeParam: interface compliance requires value receiver.
func (f *fakeMemory) Recall(ctx context.Context, query string) (*memory.RecallResponse, error) {
	f.gotQuery = query
	return f.recallResp, f.recallErr
}

func TestRecallTool_HappyPath(t *testing.T) {
	fake := &fakeMemory{recallResp: &memory.RecallResponse{
		Memories: []memory.MemoryItem{{Content: "X was Y"}},
	}}
	tool := NewRecallTool(fake, nil)
	got, err := tool.Execute(t.Context(), map[string]any{testQuery: "Q"})
	require.NoError(t, err)
	m, ok := got.(map[string]any)
	require.True(t, ok)
	assert.Len(t, m["memories"], 1)
	assert.Equal(t, "Q", fake.gotQuery)
}

func TestRecallTool_MissingQueryReturnsError(t *testing.T) {
	tool := NewRecallTool(&fakeMemory{}, nil)
	_, err := tool.Execute(t.Context(), map[string]any{})
	assert.ErrorContains(t, err, "missing required argument")
}

func TestRecallTool_EmptyQueryReturnsError(t *testing.T) {
	tool := NewRecallTool(&fakeMemory{}, nil)
	_, err := tool.Execute(t.Context(), map[string]any{testQuery: ""})
	assert.ErrorContains(t, err, "must not be empty")
}

func TestRecallTool_NonStringQueryReturnsError(t *testing.T) {
	tool := NewRecallTool(&fakeMemory{}, nil)
	_, err := tool.Execute(t.Context(), map[string]any{testQuery: 42})
	assert.ErrorContains(t, err, "must be a string")
}

func TestRecallTool_MemoryClientErrorWrapped(t *testing.T) {
	boom := errors.New("hindsight down")
	fake := &fakeMemory{recallErr: boom}
	tool := NewRecallTool(fake, nil)
	_, err := tool.Execute(t.Context(), map[string]any{testQuery: "Q"})
	require.Error(t, err)
	require.ErrorIs(t, err, boom)
	assert.Contains(t, err.Error(), "recall_memories:")
}

func TestRecallTool_NilMemoryPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on nil MemoryClient")
		}
	}()
	NewRecallTool(nil, nil)
}

func TestRecallTool_NilResponseReturnsEmpty(t *testing.T) {
	fake := &fakeMemory{recallResp: nil, recallErr: nil}
	tool := NewRecallTool(fake, nil)
	got, err := tool.Execute(t.Context(), map[string]any{testQuery: "Q"})
	require.NoError(t, err)
	m, ok := got.(map[string]any)
	require.True(t, ok)
	assert.Empty(t, m["memories"])
}

func TestRecallTool_NameAndDeclaration(t *testing.T) {
	tool := NewRecallTool(&fakeMemory{}, nil)
	assert.Equal(t, toolNameRecallMemories, tool.Name())
	d := tool.Declaration()
	require.NotNil(t, d)
	assert.Equal(t, toolNameRecallMemories, d.Name)
	require.NotNil(t, d.Parameters)
	assert.Contains(t, d.Parameters.Properties, testQuery)
	assert.Contains(t, d.Parameters.Required, testQuery)
}
