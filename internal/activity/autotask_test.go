package activity_test

import (
	"testing"

	autotask "github.com/tphakala/go-autotask"
	"github.com/tphakala/go-autotask/autotasktest"
	"github.com/tphakala/go-autotask/entities"

	"github.com/tphakala/alfred/internal/activity"
)

func TestAddNoteActivity(t *testing.T) {
	ticket := autotasktest.TicketFixture()
	ticketID, _ := ticket.ID.Get()
	_, client := autotasktest.NewServer(t, autotasktest.WithEntity(ticket))
	activities := activity.NewAutotaskActivities(client)

	err := activities.AddNote(t.Context(), activity.AddNoteInput{
		TicketID: ticketID,
		Title:    "Auto-triage",
		Content:  "Classified as network/high",
	})
	if err != nil {
		t.Fatalf("AddNote returned unexpected error: %v", err)
	}
}

func TestSetFieldActivity(t *testing.T) {
	ticket := autotasktest.TicketFixture()
	ticketID, _ := ticket.ID.Get()
	ts, client := autotasktest.NewServer(t, autotasktest.WithEntity(ticket))
	activities := activity.NewAutotaskActivities(client)

	beforeCount := ts.RequestCount()

	err := activities.SetField(t.Context(), activity.SetFieldInput{
		TicketID:  ticketID,
		FieldName: "priority",
		Value:     int64(1),
	})
	if err != nil {
		t.Fatalf("SetField returned unexpected error: %v", err)
	}

	afterCount := ts.RequestCount()
	if afterCount <= beforeCount {
		t.Errorf("expected at least one request to be sent, got %d new requests", afterCount-beforeCount)
	}
}

func TestGetTicketActivity(t *testing.T) {
	ticket := autotasktest.TicketFixture(func(tk *entities.Ticket) {
		tk.Title = autotask.Set("Test ticket")
		tk.Description = autotask.Set("Something is broken")
	})
	ticketID, _ := ticket.ID.Get()
	_, client := autotasktest.NewServer(t, autotasktest.WithEntity(ticket))
	activities := activity.NewAutotaskActivities(client)

	result, err := activities.GetTicket(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("GetTicket returned unexpected error: %v", err)
	}

	if result.Title != "Test ticket" {
		t.Errorf("expected title %q, got %q", "Test ticket", result.Title)
	}
	if result.Description != "Something is broken" {
		t.Errorf("expected description %q, got %q", "Something is broken", result.Description)
	}
}
