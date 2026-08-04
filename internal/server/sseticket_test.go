package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestSSETicketSingleUseAndExpiry(t *testing.T) {
	store := newTicketStore(30 * time.Second)
	tok, _ := store.mint()
	if tok == "" {
		t.Fatal("empty ticket")
	}
	if !store.redeem(tok) {
		t.Fatal("first redeem should succeed")
	}
	if store.redeem(tok) {
		t.Fatal("second redeem must fail (single use)")
	}

	expired := newTicketStore(-1 * time.Second) // already expired
	expTok, _ := expired.mint()
	if expired.redeem(expTok) {
		t.Fatal("expired ticket must not redeem")
	}
}

func TestHandleCreateSSETicket(t *testing.T) {
	s := &Server{tickets: newTicketStore(30 * time.Second)}
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/auth/sse-tickets", http.NoBody)
	rec := httptest.NewRecorder()
	s.handleCreateSSETicket(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusCreated)
	}
	var body struct {
		Ticket    string `json:"ticket"`
		ExpiresAt string `json:"expiresAt"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Ticket == "" {
		t.Fatal("empty ticket in response")
	}
	if body.ExpiresAt == "" {
		t.Fatal("empty expiresAt in response")
	}
	if _, err := time.Parse(time.RFC3339, body.ExpiresAt); err != nil {
		t.Fatalf("expiresAt not RFC3339: %v", err)
	}
	// The minted ticket must be redeemable exactly once.
	if !s.tickets.redeem(body.Ticket) {
		t.Fatal("minted ticket should redeem")
	}
}
