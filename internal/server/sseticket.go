package server

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

// sseTicketTTL is how long a minted SSE ticket stays redeemable.
const sseTicketTTL = 30 * time.Second

// ticketBytes is the number of random bytes in an SSE ticket.
const ticketBytes = 32

type ticketStore struct {
	mu     sync.Mutex
	ttl    time.Duration
	issued map[string]time.Time
}

func newTicketStore(ttl time.Duration) *ticketStore {
	return &ticketStore{ttl: ttl, issued: make(map[string]time.Time)}
}

// mint returns a new single-use token and its expiry time. The empty token and
// zero time are returned if entropy is unavailable.
func (s *ticketStore) mint() (string, time.Time) {
	b := make([]byte, ticketBytes)
	if _, err := rand.Read(b); err != nil {
		return "", time.Time{}
	}
	tok := base64.RawURLEncoding.EncodeToString(b)
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	// Opportunistic sweep: drop already-expired tickets so the map cannot grow
	// unbounded from minted-but-never-redeemed tokens.
	for k, e := range s.issued {
		if now.After(e) {
			delete(s.issued, k)
		}
	}
	exp := now.Add(s.ttl)
	s.issued[tok] = exp
	return tok, exp
}

// redeem returns true exactly once for a valid, unexpired ticket, then deletes it.
func (s *ticketStore) redeem(tok string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	exp, ok := s.issued[tok]
	if !ok {
		return false
	}
	delete(s.issued, tok)
	return time.Now().Before(exp)
}

// handleCreateSSETicket mints a ticket. It is mounted behind bearer auth.
func (s *Server) handleCreateSSETicket(w http.ResponseWriter, _ *http.Request) {
	tok, exp := s.tickets.mint()
	if tok == "" {
		writeProblem(w, http.StatusInternalServerError, "failed to mint ticket")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = writeJSON(w, map[string]any{
		"ticket":    tok,
		"expiresAt": exp.UTC().Format(time.RFC3339),
	})
}

// writeJSON encodes v as JSON to w. The caller sets status and content-type.
func writeJSON(w http.ResponseWriter, v any) error {
	return json.NewEncoder(w).Encode(v)
}
