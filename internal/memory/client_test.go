package memory

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRetain(t *testing.T) {
	var gotBody RetainRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method: got %q, want POST", r.Method)
		}
		if r.URL.Path != "/retain" {
			t.Errorf("path: got %q, want /retain", r.URL.Path)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		if err := json.Unmarshal(body, &gotBody); err != nil {
			t.Fatalf("unmarshal body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "test-bank")
	err := client.Retain(t.Context(), RetainRequest{
		Content: "some memory content",
	})
	if err != nil {
		t.Fatalf("Retain returned unexpected error: %v", err)
	}
	if gotBody.Content != "some memory content" {
		t.Errorf("content: got %q, want %q", gotBody.Content, "some memory content")
	}
	if gotBody.BankID != "test-bank" {
		t.Errorf("bank_id: got %q, want %q", gotBody.BankID, "test-bank")
	}
}

//nolint:gocognit,gocyclo // test function with many assertions is inherently complex
func TestRecall(t *testing.T) {
	mockResp := RecallResponse{
		Memories: []MemoryItem{
			{Content: "first memory", Score: 0.95, Tags: []string{"tag1"}},
			{Content: "second memory", Score: 0.80},
		},
	}

	var gotBody RecallRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method: got %q, want POST", r.Method)
		}
		if r.URL.Path != "/recall" {
			t.Errorf("path: got %q, want /recall", r.URL.Path)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		if err := json.Unmarshal(body, &gotBody); err != nil {
			t.Fatalf("unmarshal body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(mockResp); err != nil {
			t.Fatalf("encode response: %v", err)
		}
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "test-bank")
	resp, err := client.Recall(t.Context(), "what do I know about Go?")
	if err != nil {
		t.Fatalf("Recall returned unexpected error: %v", err)
	}

	// Verify request body sent to server.
	if gotBody.Query != "what do I know about Go?" {
		t.Errorf("query: got %q, want %q", gotBody.Query, "what do I know about Go?")
	}
	if gotBody.BankID != "test-bank" {
		t.Errorf("bank_id: got %q, want %q", gotBody.BankID, "test-bank")
	}
	if gotBody.Limit != 10 {
		t.Errorf("limit: got %d, want 10", gotBody.Limit)
	}

	// Verify parsed response.
	if len(resp.Memories) != 2 {
		t.Fatalf("memories count: got %d, want 2", len(resp.Memories))
	}
	if resp.Memories[0].Content != "first memory" {
		t.Errorf("memories[0].Content: got %q, want %q", resp.Memories[0].Content, "first memory")
	}
	if resp.Memories[0].Score != 0.95 {
		t.Errorf("memories[0].Score: got %f, want 0.95", resp.Memories[0].Score)
	}
	if len(resp.Memories[0].Tags) != 1 || resp.Memories[0].Tags[0] != "tag1" {
		t.Errorf("memories[0].Tags: got %v, want [tag1]", resp.Memories[0].Tags)
	}
	if resp.Memories[1].Content != "second memory" {
		t.Errorf("memories[1].Content: got %q, want %q", resp.Memories[1].Content, "second memory")
	}
	if resp.Memories[1].Score != 0.80 {
		t.Errorf("memories[1].Score: got %f, want 0.80", resp.Memories[1].Score)
	}
}

func TestRecallServerDown(t *testing.T) {
	// Use an address that is not listening.
	client := NewClient("http://127.0.0.1:1", "test-bank")
	resp, err := client.Recall(t.Context(), "anything")
	if err == nil {
		t.Fatal("Recall should have returned an error when server is unreachable")
	}
	if resp != nil {
		t.Errorf("response should be nil on error, got: %v", resp)
	}
}

func TestRetainServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal error", http.StatusInternalServerError)
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "test-bank")
	err := client.Retain(t.Context(), RetainRequest{Content: "anything"})
	if err == nil {
		t.Fatal("Retain should have returned an error on non-2xx response")
	}
}

func TestRecallServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "test-bank")
	resp, err := client.Recall(t.Context(), "anything")
	if err == nil {
		t.Fatal("Recall should have returned an error on non-2xx response")
	}
	if resp != nil {
		t.Errorf("response should be nil on error, got: %v", resp)
	}
}
