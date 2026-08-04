package llm

import (
	"context"
	"io"
)

// Compile-time interface satisfaction checks.
var _ Client = &MockClient{}

// Client is the interface for LLM operations.
type Client interface {
	Generate(ctx context.Context, req GenerateRequest) (*GenerateResponse, error)
	Stream(ctx context.Context, req StreamRequest) (StreamIterator, error)
	Close() error
}

// GenerateRequest holds parameters for a content generation call.
type GenerateRequest struct {
	Model       string
	Prompt      string
	Temperature float32
	MaxTokens   int32
}

// GenerateResponse holds the result of a content generation call.
type GenerateResponse struct {
	Content string
	Tokens  TokenUsage
}

// TokenUsage tracks token consumption for a generation call.
type TokenUsage struct {
	PromptTokens   int32
	ResponseTokens int32
	TotalTokens    int32
}

// MockClient implements Client for testing. It is exported so other packages
// can use it in their own tests.
type MockClient struct {
	Response          string
	Tokens            TokenUsage
	Err               error
	MockStream        []StreamChunk // Chunks returned by Stream, in order.
	StreamErr         error         // If non-nil, Stream returns this before any chunk.
	StreamFunc        func(ctx context.Context, req StreamRequest) (StreamIterator, error)
	LastStreamRequest *StreamRequest
}

// Generate returns the pre-configured response or error.
func (m *MockClient) Generate(_ context.Context, _ GenerateRequest) (*GenerateResponse, error) {
	if m.Err != nil {
		return nil, m.Err
	}
	return &GenerateResponse{Content: m.Response, Tokens: m.Tokens}, nil
}

// Close is a no-op for the mock client.
func (m *MockClient) Close() error { return nil }

// mockStreamIterator yields pre-configured chunks from a slice.
type mockStreamIterator struct {
	chunks []StreamChunk
	pos    int
	closed bool
}

func (i *mockStreamIterator) Next() (StreamChunk, error) {
	if i.closed {
		return StreamChunk{}, io.EOF
	}
	if i.pos >= len(i.chunks) {
		return StreamChunk{}, io.EOF
	}
	c := i.chunks[i.pos]
	i.pos++
	return c, nil
}

func (i *mockStreamIterator) Close() error {
	i.closed = true
	return nil
}

// Stream returns an iterator over the pre-configured MockStream chunks. If
// StreamFunc is set, it delegates entirely to that function. If StreamErr is
// non-nil, Stream returns it before producing any chunks. The request is
// always captured in LastStreamRequest for test assertions.
func (m *MockClient) Stream(ctx context.Context, req StreamRequest) (StreamIterator, error) { //nolint:gocritic // hugeParam: req must match Client interface signature.
	m.LastStreamRequest = &req
	if m.StreamFunc != nil {
		return m.StreamFunc(ctx, req)
	}
	if m.StreamErr != nil {
		return nil, m.StreamErr
	}
	return &mockStreamIterator{chunks: m.MockStream}, nil
}
