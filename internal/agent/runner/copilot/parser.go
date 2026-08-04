package copilot

import (
	"encoding/json"

	"github.com/tphakala/alfred/internal/agent/runner"
)

// ParseLine converts one JSONL line from copilot --output-format json into a
// normalized runner.Event. Unknown event types return Event{Kind:KindUnknown}
// rather than dropping the line; this preserves the raw stream for debugging
// when Copilot ships a new event type unannounced.
//
// Returns ok=false only when the line is not valid JSON at all.
func ParseLine(line []byte) (runner.Event, bool) {
	var head struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(line, &head); err != nil {
		return runner.Event{}, false
	}

	switch head.Type {
	case "session_start":
		return runner.Event{Kind: runner.KindSessionStart, Raw: json.RawMessage(line)}, true

	case "message_delta":
		var env struct {
			Delta struct {
				Text string `json:"text"`
			} `json:"delta"`
		}
		if err := json.Unmarshal(line, &env); err != nil {
			return runner.Event{Kind: runner.KindUnknown, Raw: json.RawMessage(line)}, true
		}
		return runner.Event{Kind: runner.KindTextDelta, Text: env.Delta.Text, Raw: json.RawMessage(line)}, true

	case "tool_call":
		var env struct {
			CallID string          `json:"call_id"`
			Tool   string          `json:"tool"`
			Args   json.RawMessage `json:"args"`
		}
		if err := json.Unmarshal(line, &env); err != nil {
			return runner.Event{Kind: runner.KindUnknown, Raw: json.RawMessage(line)}, true
		}
		return runner.Event{
			Kind:   runner.KindToolCall,
			CallID: env.CallID,
			Tool:   env.Tool,
			Args:   env.Args,
			Raw:    json.RawMessage(line),
		}, true

	case "tool_result":
		var env struct {
			CallID string          `json:"call_id"`
			Result json.RawMessage `json:"result"`
		}
		if err := json.Unmarshal(line, &env); err != nil {
			return runner.Event{Kind: runner.KindUnknown, Raw: json.RawMessage(line)}, true
		}
		return runner.Event{
			Kind:   runner.KindToolResult,
			CallID: env.CallID,
			Result: env.Result,
			Raw:    json.RawMessage(line),
		}, true

	case "complete":
		var env doneEnvelope
		if err := json.Unmarshal(line, &env); err != nil {
			return runner.Event{Kind: runner.KindUnknown, Raw: json.RawMessage(line)}, true
		}
		return runner.Event{Kind: runner.KindDone, UsageDelta: env.Usage, Raw: json.RawMessage(line)}, true
	}

	return runner.Event{Kind: runner.KindUnknown, Raw: json.RawMessage(line)}, true
}

// doneEnvelope carries the fields Run consumes from copilot's "complete"
// event. The CLI's envelope has no cost figure, so RunResult.TotalCostUSD
// stays zero for copilot runs.
type doneEnvelope struct {
	Usage runner.TokenUsage `json:"usage"`
}
