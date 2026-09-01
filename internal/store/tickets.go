package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// Priorities a tenant may set. Validated against a fixed set so the operator's
// filters cannot be polluted by arbitrary strings.
var ticketPriorities = map[string]bool{"low": true, "normal": true, "urgent": true}

// Ticket is a repair request. The backoffice calls these tickets; the tenant
// sees them as แจ้งซ่อม.
type Ticket struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Detail    string `json:"detail,omitempty"`
	Priority  string `json:"priority"`
	Status    string `json:"status"`
	CreatedAt string `json:"created_at"`
	ClosedAt  string `json:"closed_at,omitempty"`
}

const ticketSelect = `
	SELECT k.id, k.title, k.detail, k.priority, k.status, k.created_at, k.closed_at
	FROM tickets k
	JOIN tenants t ON t.id = k.tenant_id
	WHERE t.user_id = ?1`

func (s *Store) TicketsForUser(ctx context.Context, userID string) ([]Ticket, error) {
	res, err := s.db.Query(ctx, ticketSelect+` ORDER BY k.created_at DESC`, userID)
	if err != nil {
		return nil, fmt.Errorf("store: list tickets: %w", err)
	}

	tickets := make([]Ticket, 0, len(res.Results))
	for _, row := range res.Results {
		tickets = append(tickets, scanTicket(row))
	}
	return tickets, nil
}

func (s *Store) TicketForUser(ctx context.Context, userID, ticketID string) (*Ticket, error) {
	res, err := s.db.Query(ctx, ticketSelect+` AND k.id = ?2`, userID, ticketID)
	if err != nil {
		return nil, fmt.Errorf("store: get ticket: %w", err)
	}
	if len(res.Results) == 0 {
		return nil, ErrNotFound
	}
	ticket := scanTicket(res.Results[0])
	return &ticket, nil
}

// CreateTicket files a repair request against the caller's own room.
//
// One statement, because D1 permits no parameterised multi-statement write. The
// room and tenant are read from the caller's active contract inside the INSERT;
// nothing about where the request is filed comes from the request body.
func (s *Store) CreateTicket(ctx context.Context, userID, title, detail, priority string) (*Ticket, error) {
	title = strings.TrimSpace(title)
	detail = strings.TrimSpace(detail)

	if title == "" || len([]rune(title)) > 120 {
		return nil, fmt.Errorf("%w: title", ErrInvalid)
	}
	if len([]rune(detail)) > 2000 {
		return nil, fmt.Errorf("%w: detail", ErrInvalid)
	}
	if priority == "" {
		priority = "normal"
	}
	if !ticketPriorities[priority] {
		return nil, fmt.Errorf("%w: priority", ErrInvalid)
	}

	id := uuid.NewString()

	res, err := s.db.Query(ctx, `
		INSERT INTO tickets (id, room_id, tenant_id, title, detail, priority)
		SELECT ?1, c.room_id, t.id, ?2, ?3, ?4
		FROM tenants t
		JOIN contracts c ON c.tenant_id = t.id AND c.status = 'active'
		WHERE t.user_id = ?5
		ORDER BY c.start_date DESC
		LIMIT 1`, id, title, detail, priority, userID)
	if err != nil {
		return nil, fmt.Errorf("store: create ticket: %w", err)
	}
	if res.Meta.Changes == 0 {
		// The SELECT matched nothing, so the caller has no active contract and
		// therefore no room to report against.
		return nil, ErrNotFound
	}

	return s.TicketForUser(ctx, userID, id)
}

func scanTicket(row map[string]any) Ticket {
	return Ticket{
		ID:        text(row["id"]),
		Title:     text(row["title"]),
		Detail:    text(row["detail"]),
		Priority:  text(row["priority"]),
		Status:    text(row["status"]),
		CreatedAt: text(row["created_at"]),
		ClosedAt:  text(row["closed_at"]),
	}
}
