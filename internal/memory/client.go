package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// MemoryClient is the interface satisfied by Client and any test double.
type MemoryClient interface {
	Retain(ctx context.Context, req RetainRequest) error
	Recall(ctx context.Context, query string) (*RecallResponse, error)
}

// Client wraps the Hindsight REST API.
type Client struct {
	baseURL string
	bankID  string
	http    *http.Client
}

// RetainRequest is the payload sent to POST /retain.
type RetainRequest struct {
	Content  string            `json:"content"`
	BankID   string            `json:"bank_id,omitempty"`
	Tags     []string          `json:"tags,omitempty"`
	Metadata map[string]string `json:"metadata,omitempty"`
	Context  string            `json:"context,omitempty"`
}

// RecallRequest is the payload sent to POST /recall.
type RecallRequest struct {
	Query  string `json:"query"`
	BankID string `json:"bank_id,omitempty"`
	Limit  int    `json:"limit,omitempty"`
}

// RecallResponse holds the memories returned by POST /recall.
type RecallResponse struct {
	Memories []MemoryItem `json:"memories"`
}

// MemoryItem represents a single recalled memory with its relevance score.
type MemoryItem struct {
	Content string   `json:"content"`
	Score   float64  `json:"score"`
	Tags    []string `json:"tags,omitempty"`
}

const defaultRecallLimit = 10

// NewClient creates a Client that stores memories in the given bank.
func NewClient(baseURL, bankID string) *Client {
	return &Client{
		baseURL: baseURL,
		bankID:  bankID,
		http:    &http.Client{},
	}
}

// Retain stores a memory via POST /retain. The client's bankID is always set on
// the request, overriding any value the caller may have placed in req.BankID.
//
//nolint:gocritic // hugeParam: RetainRequest is modified locally (BankID overwrite); pointer would change the interface
func (c *Client) Retain(ctx context.Context, req RetainRequest) error {
	req.BankID = c.bankID

	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("memory: marshal retain request: %w", err)
	}

	endpoint, err := url.JoinPath(c.baseURL, "retain")
	if err != nil {
		return fmt.Errorf("memory: build retain URL: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("memory: build retain request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return fmt.Errorf("memory: retain: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		errBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("memory: retain: status %d: %s", resp.StatusCode, errBody)
	}
	return nil
}

// Recall retrieves the top 10 memories most relevant to query via POST /recall.
func (c *Client) Recall(ctx context.Context, query string) (*RecallResponse, error) {
	req := RecallRequest{
		Query:  query,
		BankID: c.bankID,
		Limit:  defaultRecallLimit,
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("memory: marshal recall request: %w", err)
	}

	endpoint, err := url.JoinPath(c.baseURL, "recall")
	if err != nil {
		return nil, fmt.Errorf("memory: build recall URL: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("memory: build recall request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("memory: recall: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		errBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("memory: recall: status %d: %s", resp.StatusCode, errBody)
	}

	var result RecallResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("memory: decode recall response: %w", err)
	}
	return &result, nil
}
