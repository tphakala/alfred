package server

import (
	"net/http"
	"strconv"
	"strings"
)

// parseLastEventID returns the resume offset from the Last-Event-ID header, or 0.
func parseLastEventID(r *http.Request) int {
	v := r.Header.Get("Last-Event-ID")
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// writeSSEFrame writes one SSE frame with an id (sequence), event type, and data.
// CR/LF are stripped from the type; data newlines are folded into continuation lines.
func writeSSEFrame(w http.ResponseWriter, sequence int, eventType, data string) {
	safeType := sseReplacer.Replace(eventType)
	safeData := strings.ReplaceAll(data, "\r", "")
	safeData = strings.ReplaceAll(safeData, "\n", "\ndata: ")
	_, _ = w.Write([]byte("id: " + strconv.Itoa(sequence) + "\nevent: " + safeType + "\ndata: " + safeData + "\n\n"))
}
