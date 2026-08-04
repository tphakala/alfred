package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWriteProblem(t *testing.T) {
	rec := httptest.NewRecorder()
	writeProblem(rec, 400, "invalid session id")
	if got := rec.Header().Get("Content-Type"); got != "application/problem+json" {
		t.Fatalf("content-type = %q", got)
	}
	if rec.Code != 400 {
		t.Fatalf("status = %d", rec.Code)
	}
	var p map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	status, _ := p["status"].(float64)
	if status != 400 || p["title"] != "invalid session id" {
		t.Fatalf("body = %v", p)
	}
}

func TestWriteProblemType(t *testing.T) {
	rec := httptest.NewRecorder()
	writeProblemType(rec, http.StatusConflict, "action not supported", "unsupported-action")
	if got := rec.Header().Get("Content-Type"); got != "application/problem+json" {
		t.Fatalf("content-type = %q", got)
	}
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d", rec.Code)
	}
	var p map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	gotType, ok := p["type"].(string)
	if !ok || gotType != "https://alfred.invalid/errors/unsupported-action" {
		t.Fatalf("type = %v", p["type"])
	}
	gotTitle, ok := p["title"].(string)
	if !ok || gotTitle != "action not supported" {
		t.Fatalf("title = %v", p["title"])
	}
}
