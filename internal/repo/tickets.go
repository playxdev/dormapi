package repo

import (
	"context"
	"fmt"
	"strings"

	"github.com/playxdev/dormapi/internal/ulid"
)

// Priorities a resident may set. Validated against a fixed set so the
// operator's filters cannot be polluted by arbitrary strings.
var ticketPriorities = map[string]bool{"low": true, "normal": true, "urgent": true}

// Ticket is a repair request. The backoffice calls these tickets; the resident
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
	SELECT k.ticket_id, k.title, k.detail, k.priority, k.status,
	       k.created_at, k.closed_at
	FROM ticket k
	WHERE k.tenant_id = ?1 AND k.party_id = ?2 AND k.deleted_at IS NULL`

func (r *Repo) Tickets(ctx context.Context, t *Tenancy) ([]Ticket, error) {
	res, err := r.db.Query(ctx, ticketSelect+` ORDER BY k.created_at DESC, k.ticket_id DESC`,
		t.tenantID, t.partyID)
	if err != nil {
		return nil, fmt.Errorf("repo: list tickets: %w", err)
	}

	tickets := make([]Ticket, 0, len(res.Results))
	for _, row := range res.Results {
		tickets = append(tickets, scanTicket(row))
	}
	return tickets, nil
}

func (r *Repo) Ticket(ctx context.Context, t *Tenancy, ticketID string) (*Ticket, error) {
	res, err := r.db.Query(ctx, ticketSelect+` AND k.ticket_id = ?3`,
		t.tenantID, t.partyID, ticketID)
	if err != nil {
		return nil, fmt.Errorf("repo: get ticket: %w", err)
	}
	if len(res.Results) == 0 {
		return nil, ErrNotFound
	}
	ticket := scanTicket(res.Results[0])
	return &ticket, nil
}

// CreateTicket files a repair request against the caller's own room.
//
// One statement. The room is read from the caller's own live lease inside the
// INSERT, so nothing about where the request lands comes from the request
// body — and the tenant_id in the INSERT is the resolved one, not a value
// that travelled with the report.
func (r *Repo) CreateTicket(ctx context.Context, t *Tenancy, title, detail, priority string) (*Ticket, error) {
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

	id := ulid.New()
	ts := now()

	res, err := r.db.Query(ctx, `
		INSERT INTO ticket
			(tenant_id, ticket_id, room_id, party_id, title, detail, priority,
			 status, created_at, updated_at)
		SELECT ?1, ?2, c.room_id, ?3, ?4, ?5, ?6, 'OPEN', ?7, ?7
		FROM contract c
		WHERE c.tenant_id = ?1 AND c.party_id = ?3
		  AND c.status IN ('ACTIVE', 'ENDING') AND c.deleted_at IS NULL
		ORDER BY c.start_date DESC
		LIMIT 1`,
		t.tenantID, id, t.partyID, title, detail, toSchema(priority), ts)
	if err != nil {
		return nil, fmt.Errorf("repo: create ticket: %w", err)
	}
	if res.Meta.Changes == 0 {
		// The SELECT matched nothing, so the caller has no live lease and
		// therefore no room to report against.
		return nil, ErrNotFound
	}

	return r.Ticket(ctx, t, id)
}

func scanTicket(row map[string]any) Ticket {
	return Ticket{
		ID:        text(row["ticket_id"]),
		Title:     text(row["title"]),
		Detail:    text(row["detail"]),
		Priority:  toWire(text(row["priority"])),
		Status:    toWire(text(row["status"])),
		CreatedAt: text(row["created_at"]),
		ClosedAt:  text(row["closed_at"]),
	}
}
