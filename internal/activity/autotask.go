package activity

import (
	"context"
	"fmt"
	"strconv"

	autotask "github.com/tphakala/go-autotask"
	"github.com/tphakala/go-autotask/entities"
)

// TicketData holds the fields of an Autotask ticket relevant to Alfred.
type TicketData struct {
	ID          int64
	Title       string
	Description string
	Status      int64
	Priority    int64
	QueueID     int64
}

// AddNoteInput is the input for the AddNote activity.
type AddNoteInput struct {
	TicketID int64
	Title    string
	Content  string
}

// SetFieldInput is the input for the SetField activity.
type SetFieldInput struct {
	TicketID  int64
	FieldName string
	Value     any
}

// Note type and publish constants used when creating ticket notes.
const (
	noteTypeInternal = int64(1)
	notePublishAll   = int64(1)
)

// AutotaskActivities groups Autotask-related Temporal activity implementations.
type AutotaskActivities struct {
	client *autotask.Client
}

// NewAutotaskActivities returns a new AutotaskActivities backed by the given client.
func NewAutotaskActivities(client *autotask.Client) *AutotaskActivities {
	return &AutotaskActivities{client: client}
}

// GetTicket fetches a ticket by ID and returns its data.
func (a *AutotaskActivities) GetTicket(ctx context.Context, ticketID int64) (*TicketData, error) {
	ticket, err := autotask.Get[entities.Ticket](ctx, a.client, ticketID)
	if err != nil {
		return nil, fmt.Errorf("activity: get ticket %d: %w", ticketID, err)
	}

	data := &TicketData{}
	if v, ok := ticket.ID.Get(); ok {
		data.ID = v
	}
	if v, ok := ticket.Title.Get(); ok {
		data.Title = v
	}
	if v, ok := ticket.Description.Get(); ok {
		data.Description = v
	}
	if v, ok := ticket.Status.Get(); ok {
		data.Status = v
	}
	if v, ok := ticket.Priority.Get(); ok {
		data.Priority = v
	}
	if v, ok := ticket.QueueID.Get(); ok {
		data.QueueID = v
	}

	return data, nil
}

// AddNote creates a ticket note under the given ticket.
func (a *AutotaskActivities) AddNote(ctx context.Context, input AddNoteInput) error {
	note := &entities.TicketNote{
		Title:       autotask.Set(input.Title),
		Description: autotask.Set(input.Content),
		NoteType:    autotask.Set(noteTypeInternal),
		Publish:     autotask.Set(notePublishAll),
	}

	_, err := autotask.CreateChild[entities.Ticket](ctx, a.client, input.TicketID, note)
	if err != nil {
		return fmt.Errorf("activity: add note to ticket %d: %w", input.TicketID, err)
	}
	return nil
}

// toInt64 coerces v to int64. It accepts int64 directly, or a string that can
// be parsed as a base-10 integer. Returns an error for any other type or an
// unparseable string.
func toInt64(v any) (int64, error) {
	switch val := v.(type) {
	case int64:
		return val, nil
	case string:
		n, err := strconv.ParseInt(val, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("cannot convert string %q to int64: %w", val, err)
		}
		return n, nil
	default:
		return 0, fmt.Errorf("expected int64 or string, got %T", v)
	}
}

// SetField updates a single named field on a ticket.
// Supported field names: "priority", "status", "queueID".
// The Value in SetFieldInput may be int64 or a string representation of an
// integer; strings are parsed automatically to support template-rendered values.
func (a *AutotaskActivities) SetField(ctx context.Context, input SetFieldInput) error {
	ticket := &entities.Ticket{
		ID: autotask.Set(input.TicketID),
	}

	switch input.FieldName {
	case "priority":
		v, err := toInt64(input.Value)
		if err != nil {
			return fmt.Errorf("activity: SetField priority: %w", err)
		}
		ticket.Priority = autotask.Set(v)
	case "status":
		v, err := toInt64(input.Value)
		if err != nil {
			return fmt.Errorf("activity: SetField status: %w", err)
		}
		ticket.Status = autotask.Set(v)
	case "queueID":
		v, err := toInt64(input.Value)
		if err != nil {
			return fmt.Errorf("activity: SetField queueID: %w", err)
		}
		ticket.QueueID = autotask.Set(v)
	default:
		return fmt.Errorf("activity: SetField: unsupported field %q", input.FieldName)
	}

	_, err := autotask.Update(ctx, a.client, ticket)
	if err != nil {
		return fmt.Errorf("activity: set field %q on ticket %d: %w", input.FieldName, input.TicketID, err)
	}
	return nil
}
