package activity_test

import (
	"fmt"
	"testing"

	"github.com/tphakala/alfred/internal/activity"
	"github.com/tphakala/alfred/internal/llm"
)

const testModelGeminiFlash = "gemini-flash"

func TestAnalyzeActivity(t *testing.T) {
	mock := &llm.MockClient{
		Response: `{"category": "network", "priority": "high", "reasoning": "server unreachable"}`,
		Tokens:   llm.TokenUsage{PromptTokens: 50, ResponseTokens: 20, TotalTokens: 70},
	}
	activities := activity.NewLLMActivities(mock)

	result, err := activities.Analyze(t.Context(), activity.AnalyzeInput{
		Model:  testModelGeminiFlash,
		Prompt: "Analyze this ticket: server down",
	})
	if err != nil {
		t.Fatalf("Analyze returned unexpected error: %v", err)
	}

	if result.Fields["category"] != "network" {
		t.Errorf("expected category %q, got %v", "network", result.Fields["category"])
	}
	if result.Fields["priority"] != "high" {
		t.Errorf("expected priority %q, got %v", "high", result.Fields["priority"])
	}
	if result.Tokens.TotalTokens != 70 {
		t.Errorf("expected TotalTokens 70, got %d", result.Tokens.TotalTokens)
	}
}

func TestAnalyzeActivity_InvalidJSON(t *testing.T) {
	mock := &llm.MockClient{
		Response: `not valid json`,
		Tokens:   llm.TokenUsage{},
	}
	activities := activity.NewLLMActivities(mock)

	_, err := activities.Analyze(t.Context(), activity.AnalyzeInput{
		Model:  testModelGeminiFlash,
		Prompt: "Analyze this ticket",
	})
	if err == nil {
		t.Fatal("expected error for invalid JSON response, got nil")
	}
}

func TestAnalyzeActivity_LLMError(t *testing.T) {
	mock := &llm.MockClient{
		Err: fmt.Errorf("upstream LLM failure"),
	}
	activities := activity.NewLLMActivities(mock)

	_, err := activities.Analyze(t.Context(), activity.AnalyzeInput{
		Model:  testModelGeminiFlash,
		Prompt: "Analyze this ticket",
	})
	if err == nil {
		t.Fatal("expected error from LLM client, got nil")
	}
}
