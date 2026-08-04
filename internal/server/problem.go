package server

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// problemBaseURI namespaces machine-readable error type URIs. New error classes
// are added as new paths under this base so the set is additive.
const problemBaseURI = "https://alfred.invalid/errors/"

// problemDetails is an RFC 9457 problem document.
type problemDetails struct {
	Type   string `json:"type,omitempty"`
	Title  string `json:"title"`
	Status int    `json:"status"`
	Detail string `json:"detail,omitempty"`
}

// writeProblem writes an RFC 9457 application/problem+json error response.
func writeProblem(w http.ResponseWriter, status int, title string) {
	writeProblemType(w, status, title, "")
}

// writeProblemType writes a problem with a machine-readable type slug appended
// to problemBaseURI (empty slug omits the type field).
func writeProblemType(w http.ResponseWriter, status int, title, typeSlug string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	p := problemDetails{Title: title, Status: status}
	if typeSlug != "" {
		p.Type = problemBaseURI + typeSlug
	}
	if err := json.NewEncoder(w).Encode(p); err != nil {
		slog.Error("failed to write problem response", "error", err)
	}
}
