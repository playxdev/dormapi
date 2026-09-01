package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/playxdev/dormapi/internal/promptpay"
)

// PaymentInfo is everything the tenant needs to pay one invoice.
//
// Two payloads are offered. The full one carries the outstanding amount, which
// is what most tenants want and removes the chance of typing it wrong. The open
// one carries no amount at all, so the tenant enters what they can pay — that
// is what makes paying in instalments possible, since a payload with the amount
// embedded cannot be part-paid.
type PaymentInfo struct {
	InvoiceID      string `json:"invoice_id"`
	DueSatang      int64  `json:"due_satang"`
	PromptPayName  string `json:"promptpay_name,omitempty"`
	PayloadFull    string `json:"payload_full,omitempty"`
	PayloadOpen    string `json:"payload_open,omitempty"`
	AcceptsPartial bool   `json:"accepts_partial"`
}

// PaymentInfoForInvoice returns the QR payloads for an invoice the caller owns.
func (s *Store) PaymentInfoForInvoice(ctx context.Context, userID, invoiceID string) (*PaymentInfo, error) {
	res, err := s.db.Query(ctx, `
		SELECT i.id, i.total,
		       COALESCE((SELECT SUM(amount) FROM payments
		                 WHERE invoice_id = i.id AND verified = 1), 0) AS paid,
		       b.promptpay_id, b.promptpay_name
		FROM invoices i
		JOIN tenants t   ON t.id = i.tenant_id
		JOIN buildings b ON b.id = i.building_id
		WHERE t.user_id = ?1 AND i.id = ?2 AND i.status NOT IN ('draft', 'void')`,
		userID, invoiceID)
	if err != nil {
		return nil, fmt.Errorf("store: payment info: %w", err)
	}
	if len(res.Results) == 0 {
		return nil, ErrNotFound
	}

	row := res.Results[0]
	due := number(row["total"]) - number(row["paid"])
	if due < 0 {
		due = 0
	}

	info := &PaymentInfo{
		InvoiceID:      text(row["id"]),
		DueSatang:      due,
		PromptPayName:  text(row["promptpay_name"]),
		AcceptsPartial: true,
	}

	// A building with no PromptPay ID configured yields no payloads rather than
	// a broken QR. The screen then tells the tenant to contact the owner.
	if id := text(row["promptpay_id"]); id != "" {
		if payload, ok := promptpay.Payload(id, due); ok {
			info.PayloadFull = payload
		}
		if payload, ok := promptpay.Payload(id, 0); ok {
			info.PayloadOpen = payload
		}
	}
	return info, nil
}

// ReportPayment records that the tenant says they paid.
//
// It is a claim, not a fact: the money went to the owner's bank and this system
// cannot see it. The row is written unverified and does not count towards the
// invoice's balance until the owner confirms it against their statement.
//
// idempotencyKey makes a retry a no-op. A tenant on a bad connection will tap
// twice, and two payment rows would look like two transfers.
func (s *Store) ReportPayment(ctx context.Context, userID, invoiceID string, amountSatang int64, method, ref, idempotencyKey string) error {
	if amountSatang <= 0 {
		return fmt.Errorf("%w: amount", ErrInvalid)
	}
	switch method {
	case "":
		method = "promptpay"
	case "promptpay", "transfer", "cash":
	default:
		return fmt.Errorf("%w: method", ErrInvalid)
	}
	ref = strings.TrimSpace(ref)
	if len([]rune(ref)) > 64 {
		return fmt.Errorf("%w: ref", ErrInvalid)
	}
	if strings.TrimSpace(idempotencyKey) == "" {
		return fmt.Errorf("%w: idempotency_key", ErrInvalid)
	}

	// One statement: the invoice is reached through the caller's tenant record,
	// so a payment cannot be reported against someone else's invoice, and D1
	// allows no parameterised multi-statement write anyway.
	res, err := s.db.Query(ctx, `
		INSERT INTO payments
			(id, invoice_id, amount, paid_at, method, ref, verified,
			 idempotency_key, reported_by_user_id)
		SELECT ?1, i.id, ?2, datetime('now'), ?3, ?4, 0, ?5, ?6
		FROM invoices i
		JOIN tenants t ON t.id = i.tenant_id
		WHERE t.user_id = ?7 AND i.id = ?8 AND i.status NOT IN ('draft', 'void')`,
		uuid.NewString(), amountSatang, method, ref, idempotencyKey, userID, userID, invoiceID)
	if err != nil {
		// The unique index caught a retry of a submission already recorded.
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return nil
		}
		return fmt.Errorf("store: report payment: %w", err)
	}
	if res.Meta.Changes == 0 {
		return ErrNotFound
	}
	return nil
}
