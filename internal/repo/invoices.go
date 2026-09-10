package repo

import (
	"context"
	"fmt"
	"time"
)

// Invoice is one billing period for one lease. Amounts are integer satang.
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

// invoiceSelect expects ?1 tenant_id and ?2 party_id.
//
// Only VERIFIED payments count towards what is paid. A reported one has been
// submitted but not yet accepted by the operator; showing it as settled would
// tell the resident they owe nothing while the operator still thinks
// otherwise.
//
// `paid_total` is deliberately not a column (see the schema's note on
// `invoice`): a running total cannot be kept correct without a transaction D1
// does not offer, so the balance is summed from the payment rows at read time.
//
// DRAFT and VOID invoices are excluded: a draft is not yet issued, and a void
// one has been cancelled.
const invoiceSelect = `
	SELECT i.invoice_id, i.number, i.period, i.due_date, i.issue_date,
	       i.status, i.total,
	       COALESCE((SELECT SUM(pm.amount) FROM payment pm
	                 WHERE pm.tenant_id = i.tenant_id
	                   AND pm.invoice_id = i.invoice_id
	                   AND pm.status = 'VERIFIED'
	                   AND pm.deleted_at IS NULL), 0) AS paid,
	       b.name   AS building_name,
	       r.number AS room_number
	FROM invoice i
	JOIN room r     ON r.tenant_id = i.tenant_id AND r.room_id = i.room_id
	JOIN building b ON b.tenant_id = i.tenant_id AND b.building_id = i.building_id
	WHERE i.tenant_id = ?1 AND i.party_id = ?2
	  AND i.status NOT IN ('DRAFT', 'VOID')
	  AND i.deleted_at IS NULL`

// Invoices lists every issued invoice belonging to the caller's own party.
//
// The party comes from the resolved membership, never from the request, so no
// caller can name someone else's invoice.
func (r *Repo) Invoices(ctx context.Context, t *Tenancy) ([]Invoice, error) {
	res, err := r.db.Query(ctx, invoiceSelect+` ORDER BY i.period DESC`, t.tenantID, t.partyID)
	if err != nil {
		return nil, fmt.Errorf("repo: list invoices: %w", err)
	}

	today := today()
	invoices := make([]Invoice, 0, len(res.Results))
	for _, row := range res.Results {
		invoices = append(invoices, scanInvoice(row, today))
	}
	return invoices, nil
}

// Invoice returns one invoice with its lines.
//
// Ownership is proved first; only then are the lines fetched. Three calls
// rather than one batch, because D1 refuses parameters when more than one
// statement is sent and inlining the id would mean building SQL by
// concatenation.
func (r *Repo) Invoice(ctx context.Context, t *Tenancy, invoiceID string) (*InvoiceDetail, error) {
	res, err := r.db.Query(ctx, invoiceSelect+` AND i.invoice_id = ?3`,
		t.tenantID, t.partyID, invoiceID)
	if err != nil {
		return nil, fmt.Errorf("repo: get invoice: %w", err)
	}
	if len(res.Results) == 0 {
		return nil, ErrNotFound
	}

	items, err := r.db.Query(ctx, `
		SELECT kind, label, detail, qty, unit, amount
		FROM invoice_item
		WHERE tenant_id = ?1 AND invoice_id = ?2 AND deleted_at IS NULL
		ORDER BY sort, item_id`, t.tenantID, invoiceID)
	if err != nil {
		return nil, fmt.Errorf("repo: get invoice items: %w", err)
	}

	paid, err := r.db.Query(ctx, `
		SELECT amount, paid_at, method, ref, status
		FROM payment
		WHERE tenant_id = ?1 AND invoice_id = ?2
		  AND status <> 'REJECTED' AND deleted_at IS NULL
		ORDER BY paid_at`, t.tenantID, invoiceID)
	if err != nil {
		return nil, fmt.Errorf("repo: get invoice payments: %w", err)
	}

	detail := &InvoiceDetail{
		Invoice:  scanInvoice(res.Results[0], today()),
		Items:    make([]InvoiceItem, 0, len(items.Results)),
		Payments: make([]Payment, 0, len(paid.Results)),
	}
	for _, row := range items.Results {
		qty, _ := row["qty"].(float64)
		detail.Items = append(detail.Items, InvoiceItem{
			Kind:         toWire(text(row["kind"])),
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
			Method:       toWire(text(row["method"])),
			Ref:          text(row["ref"]),
			Verified:     text(row["status"]) == "VERIFIED",
		})
	}
	return detail, nil
}

// effectiveStatus is a function of the data, never of a cron.
//
// PARTIAL and PAID are not stored (see the schema's note on `invoice`), and an
// invoice is overdue because its due date has passed, not because a job ran
// last night. This mirrors `InvoiceRepo.effectiveStatus` in the backoffice —
// the two must agree, or the operator and the resident read different words
// for the same row.
func effectiveStatus(status string, total, paid int64, dueDate, today string) string {
	if status != "UNPAID" {
		return status
	}
	if paid >= total && total > 0 {
		return "PAID"
	}
	if paid > 0 {
		return "PARTIAL"
	}
	if dueDate < today {
		return "OVERDUE"
	}
	return "UNPAID"
}

func scanInvoice(row map[string]any, today string) Invoice {
	total := number(row["total"])
	paid := number(row["paid"])
	return Invoice{
		ID:     text(row["invoice_id"]),
		Number: text(row["number"]),
		Period: text(row["period"]),
		Status: toWire(effectiveStatus(
			text(row["status"]), total, paid, text(row["due_date"]), today)),
		DueDate:      text(row["due_date"]),
		IssueDate:    text(row["issue_date"]),
		TotalSatang:  total,
		PaidSatang:   paid,
		DueSatang:    total - paid,
		BuildingName: text(row["building_name"]),
		RoomNumber:   text(row["room_number"]),
	}
}

// today is the operator's day, not UTC's.
//
// Every stored timestamp is UTC, but "is this invoice overdue" is a question
// about the calendar the resident and the operator live in. At 04:00 Bangkok
// the UTC date is still yesterday, and an invoice would read as due for
// another seven hours.
func today() string {
	return time.Now().In(bangkok).Format("2006-01-02")
}

var bangkok = mustLoad("Asia/Bangkok")

func mustLoad(name string) *time.Location {
	loc, err := time.LoadLocation(name)
	if err != nil {
		// A container without tzdata. UTC is seven hours behind, which moves
		// the boundary rather than breaking it, and is better than refusing to
		// serve.
		return time.FixedZone("ICT", 7*60*60)
	}
	return loc
}
