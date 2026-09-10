package repo

import (
	"context"
	"fmt"
	"strings"

	"github.com/playxdev/dormapi/internal/promptpay"
	"github.com/playxdev/dormapi/internal/ulid"
)

// Methods a resident may report. CARD is in the schema for what an operator
// records at the desk, and is not something the app can claim.
var paymentMethods = map[string]bool{"promptpay": true, "transfer": true, "cash": true}

// PaymentInfo is everything the resident needs to pay one invoice.
//
// Two payloads are offered. The full one carries the outstanding amount, which
// is what most residents want and removes the chance of typing it wrong. The
// open one carries no amount at all, so the resident enters what they can pay —
// that is what makes paying in instalments possible, since a payload with the
// amount embedded cannot be part-paid.
type PaymentInfo struct {
	InvoiceID      string `json:"invoice_id"`
	DueSatang      int64  `json:"due_satang"`
	PromptPayName  string `json:"promptpay_name,omitempty"`
	PayloadFull    string `json:"payload_full,omitempty"`
	PayloadOpen    string `json:"payload_open,omitempty"`
	AcceptsPartial bool   `json:"accepts_partial"`
}

// PaymentInfo returns the QR payloads for an invoice the caller owns.
//
// PromptPay is per building, because that is where the operator's account
// number lives — an operator running two buildings may bank them separately.
func (r *Repo) PaymentInfo(ctx context.Context, t *Tenancy, invoiceID string) (*PaymentInfo, error) {
	res, err := r.db.Query(ctx, `
		SELECT i.invoice_id, i.total,
		       COALESCE((SELECT SUM(pm.amount) FROM payment pm
		                 WHERE pm.tenant_id = i.tenant_id
		                   AND pm.invoice_id = i.invoice_id
		                   AND pm.status = 'VERIFIED'
		                   AND pm.deleted_at IS NULL), 0) AS paid,
		       b.promptpay_id, b.promptpay_name
		FROM invoice i
		JOIN building b ON b.tenant_id = i.tenant_id AND b.building_id = i.building_id
		WHERE i.tenant_id = ?1 AND i.party_id = ?2 AND i.invoice_id = ?3
		  AND i.status NOT IN ('DRAFT', 'VOID')
		  AND i.deleted_at IS NULL`, t.tenantID, t.partyID, invoiceID)
	if err != nil {
		return nil, fmt.Errorf("repo: payment info: %w", err)
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
		InvoiceID:      text(row["invoice_id"]),
		DueSatang:      due,
		PromptPayName:  text(row["promptpay_name"]),
		AcceptsPartial: true,
	}

	// A building with no PromptPay id configured yields no payloads rather
	// than a broken QR. The screen then tells the resident to contact the
	// operator.
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

// ReportPayment records that the resident says they paid.
//
// It is a claim, not a fact: the money went to the operator's bank and this
// system cannot see it. The row is written REPORTED and does not count towards
// the invoice's balance until the operator confirms it against their
// statement.
//
// idempotencyKey makes a retry a no-op. A resident on a bad connection will
// tap twice, and two payment rows would look like two transfers. The unique
// index behind it is `(tenant_id, idempotency_key)` — scoped, because two
// operators' residents can and will generate the same key.
func (r *Repo) ReportPayment(ctx context.Context, t *Tenancy, invoiceID string, amountSatang int64, method, ref, idempotencyKey string) error {
	if amountSatang <= 0 {
		return fmt.Errorf("%w: amount", ErrInvalid)
	}
	if method == "" {
		method = "promptpay"
	}
	if !paymentMethods[method] {
		return fmt.Errorf("%w: method", ErrInvalid)
	}
	ref = strings.TrimSpace(ref)
	if len([]rune(ref)) > 64 {
		return fmt.Errorf("%w: ref", ErrInvalid)
	}
	if strings.TrimSpace(idempotencyKey) == "" {
		return fmt.Errorf("%w: idempotency_key", ErrInvalid)
	}

	// One statement: the invoice is reached through the caller's own party, so
	// a payment cannot be reported against someone else's invoice, and D1
	// allows no parameterised multi-statement write anyway.
	ts := now()
	res, err := r.db.Query(ctx, `
		INSERT INTO payment
			(tenant_id, payment_id, invoice_id, amount, paid_at, method, ref,
			 status, reported_by_account, idempotency_key, created_at, updated_at)
		SELECT ?1, ?2, i.invoice_id, ?4, ?5, ?6, ?7, 'REPORTED', ?8, ?9, ?5, ?5
		FROM invoice i
		WHERE i.tenant_id = ?1 AND i.party_id = ?3 AND i.invoice_id = ?10
		  AND i.status NOT IN ('DRAFT', 'VOID')
		  AND i.deleted_at IS NULL`,
		t.tenantID, ulid.New(), t.partyID, amountSatang, ts, toSchema(method), ref,
		t.Account.ID, idempotencyKey, invoiceID)
	if err != nil {
		// The unique index caught a retry of a submission already recorded.
		if isUnique(err) {
			return nil
		}
		return fmt.Errorf("repo: report payment: %w", err)
	}
	if res.Meta.Changes == 0 {
		return ErrNotFound
	}
	return nil
}
