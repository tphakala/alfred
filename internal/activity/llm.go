package activity

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/tphakala/alfred/internal/llm"
)

// AnalyzeInput holds parameters for the Analyze activity.
type AnalyzeInput struct {
	Model       string
	Prompt      string
	Temperature float32
}

// AnalyzeOutput holds the result of an Analyze activity call.
type AnalyzeOutput struct {
	Fields map[string]any
	Tokens llm.TokenUsage
}

// LLMActivities groups LLM-related Temporal activity implementations.
type LLMActivities struct {
	client llm.Client
}

// NewLLMActivities returns a new LLMActivities backed by the given client.
func NewLLMActivities(client llm.Client) *LLMActivities {
	return &LLMActivities{client: client}
}

// Analyze calls the LLM with the given input and parses the JSON response into
// a map of fields, returning the fields alongside token usage metadata.
func (a *LLMActivities) Analyze(ctx context.Context, input AnalyzeInput) (*AnalyzeOutput, error) {
	resp, err := a.client.Generate(ctx, llm.GenerateRequest{
		Model:       input.Model,
		Prompt:      input.Prompt,
		Temperature: input.Temperature,
	})
	if err != nil {
		return nil, fmt.Errorf("activity: analyze: generate: %w", err)
	}

	var fields map[string]any
	if err := json.Unmarshal([]byte(resp.Content), &fields); err != nil {
		return nil, fmt.Errorf("activity: analyze: parse response JSON: %w", err)
	}

	return &AnalyzeOutput{
		Fields: fields,
		Tokens: resp.Tokens,
	}, nil
}
