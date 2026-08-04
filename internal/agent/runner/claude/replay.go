package claude

import (
	"encoding/json"

	"github.com/tphakala/alfred/internal/agent/runner"
)

// parseReplay extracts the user-injected message text from a stream-json
// system/replay event. Claude emits this after the workflow writes a
// user-message line to stdin between assistant turns. The message field
// echoes back the injected text so the run history records what the user
// said.
func parseReplay(line []byte) (runner.Event, bool) {
	var env struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(line, &env); err != nil {
		return runner.Event{}, false
	}
	return runner.Event{
		Kind: runner.KindReplay,
		Text: env.Message,
		Raw:  json.RawMessage(line),
	}, true
}
