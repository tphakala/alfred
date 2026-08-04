package llm

import (
	"errors"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testModelMock = "mock"

func TestMockClientStreamYieldsChunksInOrder(t *testing.T) {
	chunks := []StreamChunk{
		{Text: "hello "},
		{Text: "world"},
		{Text: "", Tokens: &TokenUsage{PromptTokens: 3, ResponseTokens: 2, TotalTokens: 5}},
	}
	m := &MockClient{MockStream: chunks}

	it, err := m.Stream(t.Context(), StreamRequest{
		Model:    testModelMock,
		Messages: []Message{{Role: RoleUser, Text: "hi"}},
	})
	require.NoError(t, err)
	require.NotNil(t, it)
	defer func() { _ = it.Close() }()

	var got []StreamChunk
	for {
		c, err := it.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		got = append(got, c)
	}

	assert.Equal(t, chunks, got)
}

func TestMockClientStreamReturnsStreamErr(t *testing.T) {
	wantErr := errors.New("boom")
	m := &MockClient{StreamErr: wantErr}

	_, err := m.Stream(t.Context(), StreamRequest{
		Model:    testModelMock,
		Messages: []Message{{Role: RoleUser, Text: "hi"}},
	})
	assert.ErrorIs(t, err, wantErr)
}

func TestMockClientStreamCloseIdempotent(t *testing.T) {
	m := &MockClient{MockStream: []StreamChunk{{Text: "a"}}}
	it, err := m.Stream(t.Context(), StreamRequest{
		Model:    testModelMock,
		Messages: []Message{{Role: RoleUser, Text: "hi"}},
	})
	require.NoError(t, err)
	require.NoError(t, it.Close())
	require.NoError(t, it.Close(), "Close must be idempotent")
}

func TestValidateStreamRequest(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		msgs    []Message
		wantErr error
	}{
		{
			name:    "empty slice returns ErrEmptyMessages",
			msgs:    nil,
			wantErr: ErrEmptyMessages,
		},
		{
			name:    "last message user is allowed",
			msgs:    []Message{{Role: RoleUser}, {Role: RoleUser}},
			wantErr: nil,
		},
		{
			name:    "last message model is rejected",
			msgs:    []Message{{Role: RoleUser}, {Role: RoleModel}},
			wantErr: ErrLastRoleNotUser,
		},
		{
			name:    "last message tool_call is rejected",
			msgs:    []Message{{Role: RoleUser}, {Role: RoleToolCall}},
			wantErr: ErrLastRoleNotUser,
		},
		{
			name:    "last message approval_request is rejected",
			msgs:    []Message{{Role: RoleUser}, {Role: RoleApprovalReq}},
			wantErr: ErrLastRoleNotUser,
		},
		{
			name:    "last message tool_result is allowed",
			msgs:    []Message{{Role: RoleUser}, {Role: RoleToolResult}},
			wantErr: nil,
		},
		{
			name:    "last message approval_result is allowed",
			msgs:    []Message{{Role: RoleUser}, {Role: RoleApprovalResult}},
			wantErr: nil,
		},
		{
			name:    "last message context is allowed",
			msgs:    []Message{{Role: RoleUser}, {Role: RoleContext}},
			wantErr: nil,
		},
		{
			name:    "last message error is allowed",
			msgs:    []Message{{Role: RoleUser}, {Role: RoleError}},
			wantErr: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := validateStreamRequest(tt.msgs)
			if tt.wantErr == nil {
				require.NoError(t, err)
				return
			}
			require.ErrorIs(t, err, tt.wantErr)
		})
	}
}

func TestMapRoleToVertex(t *testing.T) {
	t.Parallel()
	tests := []struct {
		role Role
		want string
	}{
		{RoleUser, "user"},
		{RoleModel, "model"},
		{RoleToolCall, "model"},
		{RoleApprovalReq, "model"},
		{RoleToolResult, "user"},
		{RoleApprovalResult, "user"},
		{RoleContext, "user"},
		{RoleError, "user"},
	}
	for _, tt := range tests {
		t.Run(string(tt.role), func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, mapRoleToVertex(tt.role))
		})
	}
}
