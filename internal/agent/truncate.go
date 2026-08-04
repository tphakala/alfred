package agent

import (
	"encoding/json"
	"unicode/utf8"
)

// maxToolResultBytes caps each tool result at 4 KB of JSON-serialized
// form before it both flows to the wire and gets appended to the LLM
// history. Without this cap, large recall_memories results would
// permanently bloat the chat history past the 128 KB total payload cap.
const maxToolResultBytes = 4096

// truncateToolResult returns the result unchanged when its JSON encoding
// fits under maxToolResultBytes. Otherwise it returns a placeholder map
// containing metadata and the first (maxToolResultBytes - 200) bytes of
// the JSON as a preview, walked back to a valid UTF-8 rune boundary so
// the preview string is always well-formed (tool results containing
// emoji or CJK would otherwise get a half-rune at the cut point).
func truncateToolResult(result any) any {
	if result == nil {
		return nil
	}
	encoded, err := json.Marshal(result)
	if err != nil || len(encoded) <= maxToolResultBytes {
		return result
	}
	const previewHead = maxToolResultBytes - 200
	// Walk back to the nearest rune start so we never emit a string
	// with a half-truncated multi-byte rune.
	cut := previewHead
	for cut > 0 && !utf8.RuneStart(encoded[cut]) {
		cut--
	}
	preview := string(encoded[:cut])
	return map[string]any{
		"truncated":      true,
		"original_bytes": len(encoded),
		"preview":        preview,
	}
}
