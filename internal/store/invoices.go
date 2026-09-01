package store

import (
	"context"
	"fmt"
)

// Invoice is one billing period for one contract. Amounts are integer satang.
type Invoice struct {
	ID           string `json:"id"`
	Number       string `json:"number"`
	Period       string `json:"period"`
	DueDate      string `json:"due_date"`
	IssueDate    string `json:"issue_date"`
	Status       string `json:"status"`
	TotalSatang  int64  `json:"total_satang"`
	PaidSatang   int64  `json:"paid_satang"`
	DueSatang    int64  `json:"due_satang"`
	BuildingName string `json:"building_name,omitempty"`
	RoomNumber   string `json:"room_number,omitempty"`
}

type InvoiceItem struct {
	Kind         string  `json:"kind"`
	Label        string  `json:"label"`
	Detail       string  `json:"detail,omitempty"`
	Qty          float64 `json:"qty"`
	Unit         string  `json:"unit,omitempty"`
	AmountSatang int64   `json:"amount_satang"`
}

type Payment struct {
	AmountSatang int64  `json:"amount_satang"`
	PaidAt       string `json:"paid_at"`
	Method       string `json:"method"`
	Ref          string `json:"ref,omitempty"`
	Verified     bool   `json:"verified"`
}

type InvoiceDetail struct {
	Invoice
	Items    []InvoiceItem `json:"items"`
	Payments []Payment     `json:"payments"`
}

// invoiceSelect expects ?1 to be the user ID.
//
// Only verified payments count towards what is paid. An unverified slip has
// been submitted but not yet accepted by the owner; showing it as settled would
// tell the tenant they owe nothing when the owner still thinks otherwise.
//
// Draft and void invoices are excluded: a draft is not yet issued, and a void
// one has been cancelled.
const invoiceSelect = `
	SELECT i.id, i.number, i.period, i.due_date, i.issue_date, i.status, i.total,
	       COALESCE((SELECT SUM(amount) FROM payments
	                 WHERE invoice_id = i.id AND verified = 1), 0) AS paid,
	       b.name   AS building_name,
	       r.number AS room_number
	FROM invoices i
	JOIN tenants t   ON t.id = i.tenant_id
	JOIN rooms r     ON r.id = i.room_id
	JOIN buildings b ON b.id = i.building_id
	WHERE t.user_id = ?1 AND i.status NOT IN ('draft', 'void')`

// InvoicesForUser lists every issued invoice belonging to the caller.
//
// The tenant record is joined in through the account rather than accepted from
// the request, so no caller can name someone else's invoice.
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

// InvoiceForUser returns one invoice with its lines.
//
// Ownership is proved first; only then are the lines fetched. Two calls rather
// than one batch, because D1 refuses parameters when more than one statement is
// sent and inlining the ID would mean building SQL by concatenation.
func (s *Store) InvoiceForUser(ctx context.Context, userID, invoiceID string) (*InvoiceDetail, error) {
	res, err := s.db.Query(ctx, invoiceSelect+` AND i.id = ?2`, userID, invoiceID)
	if err != nil {
		return nil, fmt.Errorf("store: get invoice: %w", err)
	}
	if len(res.Results) == 0 {
		return nil, ErrNotFound
	}

	items, err := s.db.Query(ctx, `
		SELECT kind, label, detail, qty, unit, amount
		FROM invoice_items WHERE invoice_id = ?1 ORDER BY sort, rowid`, invoiceID)
	if err != nil {
		return nil, fmt.Errorf("store: get invoice items: %w", err)
	}

	paid, err := s.db.Query(ctx, `
		SELECT amount, paid_at, method, ref, verified
		FROM payments WHERE invoice_id = ?1 ORDER BY paid_at`, invoiceID)
	if err != nil {
		return nil, fmt.Errorf("store: get invoice payments: %w", err)
	}

	detail := &InvoiceDetail{
		Invoice:  scanInvoice(res.Results[0]),
		Items:    make([]InvoiceItem, 0, len(items.Results)),
		Payments: make([]Payment, 0, len(paid.Results)),
	}
	for _, row := range items.Results {
		qty, _ := row["qty"].(float64)
		detail.Items = append(detail.Items, InvoiceItem{
			Kind:         text(row["kind"]),
			Label:        text(row["label"]),
			Detail:       text(row["detail"]),
			Qty:          qty,
			Unit:         text(row["unit"]),
			AmountSatang: number(row["amount"]),
		})
	}
	for _, row := range paid.Results {
		detail.Payments = append(detail.Payments, Payment{
			AmountSatang: number(row["amount"]),
			PaidAt:       text(row["paid_at"]),
			Method:       text(row["method"]),
			Ref:          text(row["ref"]),
			Verified:     number(row["verified"]) == 1,
		})
	}
	return detail, nil
}

func scanInvoice(row map[string]any) Invoice {
	total := number(row["total"])
	paid := number(row["paid"])
	return Invoice{
		ID:           text(row["id"]),
		Number:       text(row["number"]),
		Period:       text(row["period"]),
		DueDate:      text(row["due_date"]),
		IssueDate:    text(row["issue_date"]),
		Status:       text(row["status"]),
		TotalSatang:  total,
		PaidSatang:   paid,
		DueSatang:    total - paid,
		BuildingName: text(row["building_name"]),
		RoomNumber:   text(row["room_number"]),
	}
}
