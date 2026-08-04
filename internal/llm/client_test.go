package llm

import (
	"errors"
	"testing"
)

func TestMockClientGenerate(t *testing.T) {
	client := &MockClient{
		Response: `{"category": "network", "priority": "high"}`,
		Tokens:   TokenUsage{PromptTokens: 10, ResponseTokens: 20, TotalTokens: 30},
	}
	resp, err := client.Generate(t.Context(), GenerateRequest{
		Model:  "gemini-flash",
		Prompt: "Analyze this ticket",
	})
	if err != nil {
		t.Fatalf("Generate returned unexpected error: %v", err)
	}
	if resp.Content != client.Response {
		t.Errorf("Content mismatch\ngot:  %q\nwant: %q", resp.Content, client.Response)
	}
	if resp.Tokens.PromptTokens != 10 {
		t.Errorf("PromptTokens: got %d, want 10", resp.Tokens.PromptTokens)
	}
	if resp.Tokens.ResponseTokens != 20 {
		t.Errorf("ResponseTokens: got %d, want 20", resp.Tokens.ResponseTokens)
	}
	if resp.Tokens.TotalTokens != 30 {
		t.Errorf("TotalTokens: got %d, want 30", resp.Tokens.TotalTokens)
	}
}

func TestMockClientGenerateError(t *testing.T) {
	expectedErr := errors.New("mock error")
	client := &MockClient{Err: expectedErr}

	resp, err := client.Generate(t.Context(), GenerateRequest{
		Model:  "gemini-flash",
		Prompt: "Analyze this ticket",
	})
	if err == nil {
		t.Fatal("Generate should have returned an error but did not")
	}
	if !errors.Is(err, expectedErr) {
		t.Errorf("error mismatch\ngot:  %v\nwant: %v", err, expectedErr)
	}
	if resp != nil {
		t.Errorf("response should be nil on error, got: %v", resp)
	}
}

func TestMockClientClose(t *testing.T) {
	client := &MockClient{}
	if err := client.Close(); err != nil {
		t.Errorf("Close returned unexpected error: %v", err)
	}
}

// TestClientInterface verifies that MockClient satisfies the Client interface.
func TestClientInterface(t *testing.T) {
	var _ Client = &MockClient{}
}
