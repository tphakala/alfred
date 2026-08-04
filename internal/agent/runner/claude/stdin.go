package claude

import (
	"context"
	"encoding/json"
	"io"
	"log"
)

// streamGuidance writes one stream-json user-message NDJSON line into w for
// each string received from guidance, until guidance is closed or ctx is done.
// The wire shape matches Claude Code's --input-format stream-json contract:
//
//	{"type":"user","message":{"role":"user","content":[{"type":"text","text":"..."}]}}
//
// Errors writing to w are logged at warn level. They are not fatal; the
// subprocess is responsible for its own stdin lifecycle, and a closed pipe
// usually indicates the subprocess has exited.
func streamGuidance(ctx context.Context, w io.Writer, guidance <-chan string) {
	for {
		select {
		case <-ctx.Done():
			return
		case text, ok := <-guidance:
			if !ok {
				return
			}
			line, err := buildUserMessageLine(text)
			if err != nil {
				log.Printf("warn: build user message line: %v", err)
				continue
			}
			if _, err := w.Write(line); err != nil {
				log.Printf("warn: write user message to stdin: %v", err)
				return
			}
		}
	}
}

func buildUserMessageLine(text string) ([]byte, error) {
	type contentBlock struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	type message struct {
		Role    string         `json:"role"`
		Content []contentBlock `json:"content"`
	}
	type envelope struct {
		Type    string  `json:"type"`
		Message message `json:"message"`
	}
	b, err := json.Marshal(envelope{
		Type: "user",
		Message: message{
			Role:    "user",
			Content: []contentBlock{{Type: "text", Text: text}},
		},
	})
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}
