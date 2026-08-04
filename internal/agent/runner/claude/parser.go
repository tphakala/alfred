package claude

import (
	"encoding/json"
	"fmt"

	"github.com/tphakala/alfred/internal/agent/runner"
)

// ParseLine converts one NDJSON line from claude --output-format stream-json
// into a normalized runner.Event. Returns ok=false if the line is malformed or
// of an unknown type.
func ParseLine(line []byte) (runner.Event, bool) {
	var head struct {
		Type    string `json:"type"`
		Subtype string `json:"subtype"`
	}
	if err := json.Unmarshal(line, &head); err != nil {
		return runner.Event{}, false
	}

	switch head.Type {
	case "system":
		return parseSystem(line, head.Subtype)
	case "stream_event":
		return parseStreamEvent(line)
	case "assistant":
		return parseAssistant(line)
	case "user":
		return parseUser(line)
	case "result":
		return parseResult(line)
	}
	return runner.Event{}, false
}

func parseSystem(line []byte, subtype string) (runner.Event, bool) {
	switch subtype {
	case "init":
		return runner.Event{Kind: runner.KindSessionStart, Raw: json.RawMessage(line)}, true
	case "api_retry":
		var env struct {
			Attempt      int    `json:"attempt"`
			MaxRetries   int    `json:"max_retries"`
			RetryDelayMS int    `json:"retry_delay_ms"`
			ErrorStatus  string `json:"error_status"`
			Error        string `json:"error"`
		}
		if err := json.Unmarshal(line, &env); err != nil {
			return runner.Event{}, false
		}
		text := fmt.Sprintf("api_retry attempt %d/%d (%s): %s",
			env.Attempt, env.MaxRetries, env.ErrorStatus, env.Error)
		return runner.Event{Kind: runner.KindApiRetry, Text: text, Raw: json.RawMessage(line)}, true
	case "replay":
		return parseReplay(line)
	}
	return runner.Event{}, false
}

func parseStreamEvent(line []byte) (runner.Event, bool) {
	var env struct {
		Event struct {
			Type  string `json:"type"`
			Delta struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"delta"`
		} `json:"event"`
	}
	if err := json.Unmarshal(line, &env); err != nil {
		return runner.Event{}, false
	}
	if env.Event.Type != "content_block_delta" || env.Event.Delta.Type != "text_delta" {
		return runner.Event{}, false
	}
	return runner.Event{
		Kind: runner.KindTextDelta,
		Text: env.Event.Delta.Text,
		Raw:  json.RawMessage(line),
	}, true
}

func parseAssistant(line []byte) (runner.Event, bool) {
	var env struct {
		Message struct {
			Content []struct {
				Type  string          `json:"type"`
				ID    string          `json:"id"`
				Name  string          `json:"name"`
				Input json.RawMessage `json:"input"`
			} `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(line, &env); err != nil {
		return runner.Event{}, false
	}
	for _, c := range env.Message.Content {
		if c.Type == "tool_use" {
			return runner.Event{
				Kind:   runner.KindToolCall,
				CallID: c.ID,
				Tool:   c.Name,
				Args:   c.Input,
				Raw:    json.RawMessage(line),
			}, true
		}
	}
	return runner.Event{}, false
}

func parseUser(line []byte) (runner.Event, bool) {
	var env struct {
		Message struct {
			Content []struct {
				Type      string          `json:"type"`
				ToolUseID string          `json:"tool_use_id"`
				Content   json.RawMessage `json:"content"`
			} `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(line, &env); err != nil {
		return runner.Event{}, false
	}
	for _, c := range env.Message.Content {
		if c.Type == "tool_result" {
			return runner.Event{
				Kind:   runner.KindToolResult,
				CallID: c.ToolUseID,
				Result: c.Content,
				Raw:    json.RawMessage(line),
			}, true
		}
	}
	return runner.Event{}, false
}

type doneEnvelope struct {
	IsError      bool              `json:"is_error"`
	Usage        runner.TokenUsage `json:"usage"`
	TotalCostUSD float64           `json:"total_cost_usd"`
}

func parseResult(line []byte) (runner.Event, bool) {
	var env doneEnvelope
	if err := json.Unmarshal(line, &env); err != nil {
		return runner.Event{}, false
	}
	return runner.Event{
		Kind:       runner.KindDone,
		UsageDelta: env.Usage,
		Raw:        json.RawMessage(line),
	}, true
}

// ParseDoneMetrics extracts usage and total cost from a "result" stream-json
// line.
func ParseDoneMetrics(line []byte) (runner.TokenUsage, float64, error) {
	var d doneEnvelope
	if err := json.Unmarshal(line, &d); err != nil {
		return runner.TokenUsage{}, 0, err
	}
	return d.Usage, d.TotalCostUSD, nil
}
