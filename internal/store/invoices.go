package store

import (
	"context"
	"fmt"

	"github.com/playxdev/dormapi/internal/d1"
)

// Invoice is one billing period for one tenancy.
//
// Amounts are integer satang. Balance is derived, never stored, so an invoice
// and its payments cannot disagree about what is owed.
type Invoice struct {
	ID           string `json:"id"`
	Period       string `json:"period"`
	DueDate      string `json:"due_date"`
	IssuedAt     string `json:"issued_at"`
	Status       string `json:"status"`
	TotalSatang  int64  `json:"total_satang"`
	PaidSatang   int64  `json:"paid_satang"`
	DueSatang    int64  `json:"due_satang"`
	PropertyName string `json:"property_name,omitempty"`
	RoomCode     string `json:"room_code,omitempty"`
}

type InvoiceItem struct {
	Kind         string `json:"kind"`
	Description  string `json:"description"`
	AmountSatang int64  `json:"amount_satang"`
}

type Payment struct {
	AmountSatang int64  `json:"amount_satang"`
	PaidAt       string `json:"paid_at"`
	Method       string `json:"method"`
	Reference    string `json:"reference"`
}

type InvoiceDetail struct {
	Invoice
	Items    []InvoiceItem `json:"items"`
	Payments []Payment     `json:"payments"`
}

// invoiceSelect is shared by the list and detail queries so the two can never
// compute a balance differently. It expects ?1 to be the user ID.
const invoiceSelect = `
	SELECT
		i.id, i.period, i.due_date, i.issued_at, i.status,
		COALESCE((SELECT SUM(amount_satang) FROM invoice_items WHERE invoice_id = i.id), 0) AS total_satang,
		COALESCE((SELECT SUM(amount_satang) FROM payments      WHERE invoice_id = i.id), 0) AS paid_satang,
		p.name AS property_name,
		r.code AS room_code
	FROM invoices i
	JOIN properties p ON p.id = i.property_id
	JOIN rooms r      ON r.id = i.room_id
	JOIN tenancies t  ON t.id = i.tenancy_id AND t.status = 'active'
	WHERE t.user_id = ?1 AND i.status <> 'void'`

// InvoicesForUser lists every invoice belonging to the caller's active
// tenancy, newest period first.
//
// The tenancy is joined in rather than accepted from the caller: an invoice is
// only reachable through the user who owns it, so no request can name someone
// else's invoice.
func (s *Store) InvoicesForUser(ctx context.Context, userID string) ([]Invoice, error) {
	res, err := s.db.Query(ctx, invoiceSelect+` ORDER BY i.period DESC`, userID)
	if err != nil {
		return nil, fmt.Errorf("store: list invoices: %w", err)
	}

	invoices := make([]Invoice, 0, len(res.Results))
	for _, row := range res.Results {
		invoices = append(invoices, scanInvoice(row))
	}
	return invoices, nil
}

// InvoiceForUser returns one invoice with its items and payments.
//
// Ownership is proved first. Only then are the lines fetched, in one batch —
// each D1 call is a network round trip, so the two reads are not worth two of
// them.
func (s *Store) InvoiceForUser(ctx context.Context, userID, invoiceID string) (*InvoiceDetail, error) {
	res, err := s.db.Query(ctx, invoiceSelect+` AND i.id = ?2`, userID, invoiceID)
	if err != nil {
		return nil, fmt.Errorf("store: get invoice: %w", err)
	}
	if len(res.Results) == 0 {
		// Either the invoice does not exist or it is not this user's. Both are
		// reported the same way: telling the caller which would confirm the
		// existence of another tenant's invoice.
		return nil, ErrNotFound
	}

	// Params are numbered continuously across a batch, so the second statement
	// starts at ?2 even though it is the first placeholder in its own SQL.
	results, err := s.db.Batch(ctx, []d1.Statement{
		{
			SQL:    `SELECT kind, description, amount_satang FROM invoice_items WHERE invoice_id = ?1 ORDER BY sort_order, rowid`,
			Params: []any{invoiceID},
		},
		{
			SQL:    `SELECT amount_satang, paid_at, method, reference FROM payments WHERE invoice_id = ?2 ORDER BY paid_at`,
			Params: []any{invoiceID},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("store: get invoice lines: %w", err)
	}
	if len(results) < 2 {
		return nil, fmt.Errorf("store: expected 2 result sets, got %d", len(results))
	}

	detail := &InvoiceDetail{
		Invoice:  scanInvoice(res.Results[0]),
		Items:    make([]InvoiceItem, 0, len(results[0].Results)),
		Payments: make([]Payment, 0, len(results[1].Results)),
	}
	for _, row := range results[0].Results {
		detail.Items = append(detail.Items, InvoiceItem{
			Kind:         text(row["kind"]),
			Description:  text(row["description"]),
			AmountSatang: number(row["amount_satang"]),
		})
	}
	for _, row := range results[1].Results {
		detail.Payments = append(detail.Payments, Payment{
			AmountSatang: number(row["amount_satang"]),
			PaidAt:       text(row["paid_at"]),
			Method:       text(row["method"]),
			Reference:    text(row["reference"]),
		})
	}
	return detail, nil
}

func scanInvoice(row map[string]any) Invoice {
	total := number(row["total_satang"])
	paid := number(row["paid_satang"])
	return Invoice{
		ID:           text(row["id"]),
		Period:       text(row["period"]),
		DueDate:      text(row["due_date"]),
		IssuedAt:     text(row["issued_at"]),
		Status:       text(row["status"]),
		TotalSatang:  total,
		PaidSatang:   paid,
		DueSatang:    total - paid,
		PropertyName: text(row["property_name"]),
		RoomCode:     text(row["room_code"]),
	}
}
