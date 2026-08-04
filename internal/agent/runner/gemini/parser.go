package gemini

import (
	"encoding/json"

	"github.com/tphakala/alfred/internal/agent/runner"
)

// ParseLine converts one NDJSON line from gemini --output-format stream-json
// into a normalized runner.Event. Returns ok=false if the line is malformed or
// of an unknown type.
func ParseLine(line []byte) (runner.Event, bool) {
	var head struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(line, &head); err != nil {
		return runner.Event{}, false
	}

	switch head.Type {
	case "system":
		return runner.Event{Kind: runner.KindSessionStart, Raw: json.RawMessage(line)}, true

	case "text_delta":
		var env struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(line, &env); err != nil {
			return runner.Event{}, false
		}
		return runner.Event{Kind: runner.KindTextDelta, Text: env.Text, Raw: json.RawMessage(line)}, true

	case "tool_call":
		var env struct {
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		}
		if err := json.Unmarshal(line, &env); err != nil {
			return runner.Event{}, false
		}
		return runner.Event{
			Kind:   runner.KindToolCall,
			CallID: env.ID,
			Tool:   env.Name,
			Args:   env.Input,
			Raw:    json.RawMessage(line),
		}, true

	case "tool_result":
		var env struct {
			ToolCallID string          `json:"tool_call_id"`
			Content    json.RawMessage `json:"content"`
		}
		if err := json.Unmarshal(line, &env); err != nil {
			return runner.Event{}, false
		}
		return runner.Event{
			Kind:   runner.KindToolResult,
			CallID: env.ToolCallID,
			Result: env.Content,
			Raw:    json.RawMessage(line),
		}, true

	case "result":
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

	return runner.Event{}, false
}

type doneEnvelope struct {
	IsError      bool              `json:"is_error"`
	Usage        runner.TokenUsage `json:"usage"`
	TotalCostUSD float64           `json:"total_cost_usd"`
}

// ParseDoneMetrics extracts usage and total cost from a "result" stream-json
// line, mirroring the Claude adapter's contract.
func ParseDoneMetrics(line []byte) (runner.TokenUsage, float64, error) {
	var d doneEnvelope
	if err := json.Unmarshal(line, &d); err != nil {
		return runner.TokenUsage{}, 0, err
	}
	return d.Usage, d.TotalCostUSD, nil
}
