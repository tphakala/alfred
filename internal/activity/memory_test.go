package activity_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tphakala/alfred/internal/activity"
	"github.com/tphakala/alfred/internal/memory"
)

func TestRecallActivity(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewEncoder(w).Encode(memory.RecallResponse{
			Memories: []memory.MemoryItem{
				{Content: "previous similar ticket resolved by reboot", Score: 0.9},
			},
		}); err != nil {
			t.Errorf("encode response: %v", err)
		}
	}))
	defer srv.Close()

	memClient := memory.NewClient(srv.URL, "test-bank")
	activities := activity.NewMemoryActivities(memClient)

	result, err := activities.Recall(t.Context(), "server down network issue")
	if err != nil {
		t.Fatalf("Recall returned unexpected error: %v", err)
	}

	if len(result) != 1 {
		t.Fatalf("expected 1 memory, got %d", len(result))
	}
	if result[0].Content != "previous similar ticket resolved by reboot" {
		t.Errorf("expected content %q, got %q", "previous similar ticket resolved by reboot", result[0].Content)
	}
}

func TestRetainActivity(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		if err := json.NewEncoder(w).Encode(map[string]string{"status": "accepted"}); err != nil {
			t.Errorf("encode response: %v", err)
		}
	}))
	defer srv.Close()

	memClient := memory.NewClient(srv.URL, "test-bank")
	activities := activity.NewMemoryActivities(memClient)

	err := activities.Retain(t.Context(), activity.RetainInput{
		Content: "ticket 123 triaged as network/high",
		Tags:    []string{"ticket-123"},
	})
	if err != nil {
		t.Fatalf("Retain returned unexpected error: %v", err)
	}
	if !called {
		t.Error("expected HTTP handler to be called, but it was not")
	}
}
