package repo

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/playxdev/dormapi/internal/pii"
	"github.com/playxdev/dormapi/internal/ulid"
)

/*
Every statement in this package, against the real schema.

The scripted tests prove what a statement says. This one proves it is valid:
that the columns exist, that the CHECK constraints accept the values written,
that the foreign keys have parents and that the partial unique indexes bite
where they are supposed to. None of that is observable through a fake, and
Go cannot run on Workers, so without this the first place a wrong column name
would appear is a resident's screen.

The schema comes from the dormplace checkout — see schemaDir. A test that
carried its own copy would pass against a schema nobody deploys.
*/

const inviteCode = "A7K9Q2MX"

type live struct {
	t    *testing.T
	fake *liveDB
	repo *Repo

	tenantID, accountID, partyID, contractID string
	roomID, buildingID, invoiceID            string
}

// newLive builds one operator with one room, one resident record and a lease
// waiting to be claimed — the state the backoffice leaves behind when the
// owner prints a handover sheet.
func newLive(t *testing.T) *live {
	t.Helper()

	pepper, err := pii.ParsePepper(testPepper)
	if err != nil {
		t.Fatalf("parse pepper: %v", err)
	}
	fake := newLiveDB(t)

	l := &live{
		t:          t,
		fake:       fake,
		repo:       New(fake.Client(), pepper, "2011358311", "https://backoffice.example"),
		tenantID:   ulid.New(),
		accountID:  ulid.New(),
		partyID:    ulid.New(),
		contractID: ulid.New(),
		roomID:     ulid.New(),
		buildingID: ulid.New(),
		invoiceID:  ulid.New(),
	}

	ts := now()
	ownerID := ulid.New()

	fake.Exec(`INSERT INTO account (account_id, display_name, locale, status, created_at, updated_at)
	           VALUES (?, 'เจ้าของหอ', 'th', 'ACTIVE', ?, ?)`, ownerID, ts, ts)
	fake.Exec(`INSERT INTO account (account_id, display_name, locale, status, created_at, updated_at)
	           VALUES (?, 'ผู้เช่า ทดสอบ', 'th', 'ACTIVE', ?, ?)`, l.accountID, ts, ts)
	fake.Exec(`INSERT INTO tenant (tenant_id, slug, name, owner_account_id, status, vertical,
	                               locale, timezone, currency, data_region, created_at, updated_at)
	           VALUES (?, 'test-dorm', 'หอพักทดสอบ', ?, 'ACTIVE', 'DORM',
	                   'th', 'Asia/Bangkok', 'THB', 'auto', ?, ?)`, l.tenantID, ownerID, ts, ts)
	fake.Exec(`INSERT INTO building (tenant_id, building_id, name, promptpay_id, promptpay_name,
	                                 created_at, updated_at)
	           VALUES (?, ?, 'Oscar Apartment', '0812345678', 'PLAYDEVX', ?, ?)`,
		l.tenantID, l.buildingID, ts, ts)
	fake.Exec(`INSERT INTO room (tenant_id, room_id, building_id, floor, number, rent, deposit,
	                             status, created_at, updated_at)
	           VALUES (?, ?, ?, 6, '609', 450000, 900000, 'OCCUPIED', ?, ?)`,
		l.tenantID, l.roomID, l.buildingID, ts, ts)
	fake.Exec(`INSERT INTO party (tenant_id, party_id, kind, display_name, created_by,
	                              created_at, updated_at)
	           VALUES (?, ?, 'PRIMARY', 'ผู้เช่า ทดสอบ', 'STAFF', ?, ?)`,
		l.tenantID, l.partyID, ts, ts)
	fake.Exec(`INSERT INTO contract (tenant_id, contract_id, room_id, party_id, start_date,
	                                 billing_cycle, rent, deposit, status, created_at, updated_at)
	           VALUES (?, ?, ?, ?, '2026-10-01', 'MONTHLY', 450000, 900000, 'ACTIVE', ?, ?)`,
		l.tenantID, l.contractID, l.roomID, l.partyID, ts, ts)

	invitationID := ulid.New()
	fake.Exec(`INSERT INTO invitation (tenant_id, invitation_id, purpose, delivery, verification,
	                                   binding, secret_hash, secret_prefix, max_usage, used_count,
	                                   requires_approval, expires_at, status, created_by, created_at)
	           VALUES (?, ?, 'CLIENT', 'QR', 'LINE_LOGIN', 'TARGETED', ?, 'A7K9', 1, 0, 0,
	                   '2099-01-01T00:00:00.000Z', 'ACTIVE', ?, ?)`,
		l.tenantID, invitationID, pepper.Hash(ulid.NormalizeCrockford(inviteCode)), ownerID, ts)
	fake.Exec(`INSERT INTO contract_invitation (tenant_id, invitation_id, contract_id, created_at)
	           VALUES (?, ?, ?, ?)`, l.tenantID, invitationID, l.contractID, ts)

	return l
}

// claim runs the real claim, which is also how the rest of the fixture gets a
// membership and a linked party.
func (l *live) claim() {
	l.t.Helper()
	if err := l.repo.ClaimInvite(context.Background(), l.accountID, inviteCode,
		"1.0", "1.0", "req-live"); err != nil {
		l.t.Fatalf("ClaimInvite: %v", err)
	}
}

// billing adds an issued invoice with lines, one verified payment and one
// still awaiting the operator, plus a reading, a repair and a notice.
func (l *live) billing() {
	l.t.Helper()
	ts := now()

	l.fake.Exec(`INSERT INTO invoice (tenant_id, invoice_id, number, building_id, room_id,
	                                  contract_id, party_id, period, issue_date, due_date,
	                                  subtotal, total, status, created_at, updated_at)
	             VALUES (?, ?, 'INV-2026-09-0001', ?, ?, ?, ?, '2026-09', '2026-09-01',
	                     '2026-09-05', 525000, 525000, 'UNPAID', ?, ?)`,
		l.tenantID, l.invoiceID, l.buildingID, l.roomID, l.contractID, l.partyID, ts, ts)
	l.fake.Exec(`INSERT INTO invoice_item (tenant_id, item_id, invoice_id, kind, label, qty,
	                                       unit_price, amount, sort, created_at, updated_at)
	             VALUES (?, ?, ?, 'RENT', 'ค่าเช่าห้อง', 1, 450000, 450000, 0, ?, ?)`,
		l.tenantID, ulid.New(), l.invoiceID, ts, ts)
	l.fake.Exec(`INSERT INTO invoice_item (tenant_id, item_id, invoice_id, kind, label, qty, unit,
	                                       unit_price, amount, sort, created_at, updated_at)
	             VALUES (?, ?, ?, 'WATER', 'ค่าน้ำ', 7, 'หน่วย', 1800, 12600, 1, ?, ?)`,
		l.tenantID, ulid.New(), l.invoiceID, ts, ts)
	l.fake.Exec(`INSERT INTO payment (tenant_id, payment_id, invoice_id, amount, paid_at, method,
	                                  status, verified_at, created_at, updated_at)
	             VALUES (?, ?, ?, 200000, ?, 'PROMPTPAY', 'VERIFIED', ?, ?, ?)`,
		l.tenantID, ulid.New(), l.invoiceID, ts, ts, ts, ts)
	l.fake.Exec(`INSERT INTO payment (tenant_id, payment_id, invoice_id, amount, paid_at, method,
	                                  status, created_at, updated_at)
	             VALUES (?, ?, ?, 100000, ?, 'TRANSFER', 'REPORTED', ?, ?)`,
		l.tenantID, ulid.New(), l.invoiceID, ts, ts, ts)

	l.fake.Exec(`INSERT INTO meter_reading (tenant_id, reading_id, room_id, contract_id, period,
	                                        kind, prev_value, value, status, recorded_at,
	                                        created_at, updated_at)
	             VALUES (?, ?, ?, ?, '2026-09', 'WATER', 128, 135, 'RECORDED', ?, ?, ?)`,
		l.tenantID, ulid.New(), l.roomID, l.contractID, ts, ts, ts)
	// A reading taken while the room was vacant carries no lease, and is not
	// this resident's to read.
	l.fake.Exec(`INSERT INTO meter_reading (tenant_id, reading_id, room_id, period, kind,
	                                        prev_value, value, status, recorded_at,
	                                        created_at, updated_at)
	             VALUES (?, ?, ?, '2026-08', 'WATER', 120, 128, 'RECORDED', ?, ?, ?)`,
		l.tenantID, ulid.New(), l.roomID, ts, ts, ts)

	l.fake.Exec(`INSERT INTO announcement (tenant_id, announcement_id, building_id, title, body,
	                                       pinned, published_at, created_at, updated_at)
	             VALUES (?, ?, ?, 'น้ำประปาหยุดไหล', 'ปิดซ่อมท่อเมน', 1, ?, ?, ?)`,
		l.tenantID, ulid.New(), l.buildingID, ts, ts, ts)
	// A draft. Nothing outside the backoffice may read it.
	l.fake.Exec(`INSERT INTO announcement (tenant_id, announcement_id, building_id, title, body,
	                                       created_at, updated_at)
	             VALUES (?, ?, ?, 'ยังไม่เผยแพร่', 'ร่าง', ?, ?)`,
		l.tenantID, ulid.New(), l.buildingID, ts, ts)
}

// The claim, against real constraints. Every foreign key has to have a parent,
// every CHECK has to accept what is written, and the occupancy index has to
// permit exactly one live membership.
func TestLiveClaimWritesEveryRowTheLeaseNeeds(t *testing.T) {
	l := newLive(t)
	l.claim()

	party := l.fake.Row(`SELECT account_id, membership_id, linked_at FROM party
	                     WHERE tenant_id = ? AND party_id = ?`, l.tenantID, l.partyID)
	if party["account_id"] != l.accountID {
		t.Errorf("party.account_id = %v, want the caller", party["account_id"])
	}
	if party["membership_id"] == nil || party["linked_at"] == nil {
		t.Errorf("party = %v, want a membership and a link time", party)
	}

	membership := l.fake.Row(`SELECT kind, status FROM membership
	                          WHERE tenant_id = ? AND account_id = ?`, l.tenantID, l.accountID)
	if membership["kind"] != "CLIENT" || membership["status"] != "ACTIVE" {
		t.Errorf("membership = %v", membership)
	}

	// The snapshot, taken from the lease's own columns.
	contract := l.fake.Row(`SELECT confirmed_at, agreed_rent, agreed_deposit, agreed_start_date,
	                               agreed_terms_version, agreed_pdpa_version, version
	                        FROM contract WHERE tenant_id = ? AND contract_id = ?`,
		l.tenantID, l.contractID)
	if contract["agreed_rent"] != float64(450000) || contract["agreed_deposit"] != float64(900000) {
		t.Errorf("snapshot = %v", contract)
	}
	if contract["agreed_start_date"] != "2026-10-01" || contract["agreed_terms_version"] != "1.0" {
		t.Errorf("snapshot = %v", contract)
	}
	if contract["version"] != float64(2) {
		t.Errorf("contract.version = %v, want the write to have bumped it", contract["version"])
	}

	// Single use: the invitation is spent and reads as exhausted.
	invitation := l.fake.Row(`SELECT status, used_count FROM invitation WHERE tenant_id = ?`, l.tenantID)
	if invitation["status"] != "EXHAUSTED" || invitation["used_count"] != float64(1) {
		t.Errorf("invitation = %v", invitation)
	}

	// The append-only rows (rules 8 and 9).
	if r := l.fake.Row(`SELECT result FROM invitation_redemption WHERE tenant_id = ?`, l.tenantID); r == nil ||
		r["result"] != "SUCCESS" {
		t.Errorf("redemption = %v", r)
	}
	if r := l.fake.Row(`SELECT to_status, actor_type FROM membership_event WHERE tenant_id = ?`, l.tenantID); r == nil ||
		r["to_status"] != "ACTIVE" {
		t.Errorf("membership_event = %v", r)
	}
	consent := l.fake.Row(`SELECT doc_type, doc_version, action FROM consent_record WHERE tenant_id = ?`, l.tenantID)
	if consent == nil || consent["doc_type"] != "TENANT_TERMS" || consent["action"] != "ACCEPT" {
		t.Errorf("consent = %v", consent)
	}
	if r := l.fake.Row(`SELECT COUNT(*) AS n FROM audit_event WHERE tenant_id = ?`, l.tenantID); r["n"] != float64(2) {
		t.Errorf("audit rows = %v, want the membership and the redemption", r["n"])
	}
}

// A stolen handover sheet. The party is already linked, so the guard changes
// no rows and nothing else is written.
func TestLiveASecondPersonIsRefusedByTheDatabase(t *testing.T) {
	l := newLive(t)
	l.claim()

	thief := ulid.New()
	l.fake.Exec(`INSERT INTO account (account_id, display_name, locale, status, created_at, updated_at)
	             VALUES (?, 'คนอื่น', 'th', 'ACTIVE', ?, ?)`, thief, now(), now())

	err := l.repo.ClaimInvite(context.Background(), thief, inviteCode, "1.0", "1.0", "req-2")
	if !errors.Is(err, ErrAlreadyClaimed) {
		t.Fatalf("err = %v, want ErrAlreadyClaimed", err)
	}

	party := l.fake.Row(`SELECT account_id FROM party WHERE tenant_id = ? AND party_id = ?`,
		l.tenantID, l.partyID)
	if party["account_id"] != l.accountID {
		t.Errorf("the lease moved to %v", party["account_id"])
	}
	if r := l.fake.Row(`SELECT COUNT(*) AS n FROM membership WHERE tenant_id = ?`, l.tenantID); r["n"] != float64(1) {
		t.Errorf("memberships = %v, want no membership for the second caller", r["n"])
	}
}

// The same person scanning twice, or retrying after a failure halfway. Their
// own account still matches the guard, so the call finishes rather than
// refusing — and it must not write a second redemption or a second consent.
func TestLiveAResumedClaimIsIdempotent(t *testing.T) {
	l := newLive(t)
	l.claim()

	if err := l.repo.ClaimInvite(context.Background(), l.accountID, inviteCode,
		"1.0", "1.0", "req-2"); err != nil {
		t.Fatalf("the second attempt was refused: %v", err)
	}

	for table, want := range map[string]float64{
		"membership":            1,
		"invitation_redemption": 1,
	} {
		r := l.fake.Row(`SELECT COUNT(*) AS n FROM `+table+` WHERE tenant_id = ?`, l.tenantID)
		if r["n"] != want {
			t.Errorf("%s has %v rows, want %v", table, r["n"], want)
		}
	}

	// The lease keeps the terms it was confirmed under: the second attempt
	// matches nothing, because confirmed_at is no longer NULL.
	invitation := l.fake.Row(`SELECT used_count FROM invitation WHERE tenant_id = ?`, l.tenantID)
	if invitation["used_count"] != float64(1) {
		t.Errorf("used_count = %v, want the invitation spent once", invitation["used_count"])
	}
}

// What the resident sees, end to end, against the schema that stores it.
func TestLiveTheResidentReadsTheirOwnRoom(t *testing.T) {
	l := newLive(t)
	l.claim()
	l.billing()

	ctx := context.Background()
	tenancy, err := l.repo.ResolveTenancy(ctx, l.accountID)
	if err != nil {
		t.Fatalf("ResolveTenancy: %v", err)
	}
	if tenancy.RoomNumber != "609" || tenancy.BuildingName != "Oscar Apartment" {
		t.Errorf("tenancy = %+v", tenancy)
	}
	if tenancy.OperatorName != "หอพักทดสอบ" || tenancy.ContractID != l.contractID {
		t.Errorf("tenancy = %+v", tenancy)
	}

	invoices, err := l.repo.Invoices(ctx, tenancy)
	if err != nil {
		t.Fatalf("Invoices: %v", err)
	}
	if len(invoices) != 1 {
		t.Fatalf("got %d invoices, want 1", len(invoices))
	}
	// 525000 billed, 200000 verified. The 100000 still awaiting the operator
	// counts for nothing.
	if invoices[0].PaidSatang != 200000 || invoices[0].DueSatang != 325000 {
		t.Errorf("invoice = %+v", invoices[0])
	}
	if invoices[0].Status != "partial" {
		t.Errorf("status = %q, want partial", invoices[0].Status)
	}

	detail, err := l.repo.Invoice(ctx, tenancy, invoices[0].ID)
	if err != nil {
		t.Fatalf("Invoice: %v", err)
	}
	if len(detail.Items) != 2 || detail.Items[0].Kind != "rent" {
		t.Errorf("items = %+v", detail.Items)
	}
	if len(detail.Payments) != 2 {
		t.Fatalf("payments = %+v", detail.Payments)
	}
	// Both were reported in the same second, so the order is the id's — which
	// is the order they were written in.
	if !detail.Payments[0].Verified || detail.Payments[1].Verified {
		t.Errorf("payments = %+v, want one verified and one pending", detail.Payments)
	}

	info, err := l.repo.PaymentInfo(ctx, tenancy, invoices[0].ID)
	if err != nil {
		t.Fatalf("PaymentInfo: %v", err)
	}
	if info.DueSatang != 325000 || info.PayloadFull == "" || info.PayloadOpen == "" {
		t.Errorf("payment info = %+v", info)
	}

	// The vacant month's reading belongs to the room, not to this resident.
	meters, err := l.repo.Meters(ctx, tenancy)
	if err != nil {
		t.Fatalf("Meters: %v", err)
	}
	if len(meters) != 1 {
		t.Fatalf("got %d readings, want only the one on this lease: %+v", len(meters), meters)
	}
	if meters[0].Kind != "water" || meters[0].Used != 7 {
		t.Errorf("reading = %+v", meters[0])
	}

	// The draft notice is invisible.
	notices, err := l.repo.Announcements(ctx, tenancy)
	if err != nil {
		t.Fatalf("Announcements: %v", err)
	}
	if len(notices) != 1 || notices[0].Read {
		t.Fatalf("notices = %+v, want one unread published notice", notices)
	}
	if err := l.repo.MarkAnnouncementRead(ctx, tenancy, notices[0].ID); err != nil {
		t.Fatalf("MarkAnnouncementRead: %v", err)
	}
	// Marking is idempotent: the app marks on every view.
	if err := l.repo.MarkAnnouncementRead(ctx, tenancy, notices[0].ID); err != nil {
		t.Fatalf("marking twice failed: %v", err)
	}
	notices, _ = l.repo.Announcements(ctx, tenancy)
	if !notices[0].Read {
		t.Error("the notice is still unread after being opened")
	}
}

// A repair filed by the resident, then read back.
func TestLiveARepairLandsOnTheCallersRoom(t *testing.T) {
	l := newLive(t)
	l.claim()

	ctx := context.Background()
	tenancy, err := l.repo.ResolveTenancy(ctx, l.accountID)
	if err != nil {
		t.Fatalf("ResolveTenancy: %v", err)
	}

	ticket, err := l.repo.CreateTicket(ctx, tenancy, "ไฟไม่ติด", "หลอดไฟห้องน้ำ", "urgent")
	if err != nil {
		t.Fatalf("CreateTicket: %v", err)
	}
	if ticket.Status != "open" || ticket.Priority != "urgent" {
		t.Errorf("ticket = %+v", ticket)
	}

	row := l.fake.Row(`SELECT room_id, party_id, status, priority FROM ticket WHERE tenant_id = ?`, l.tenantID)
	if row["room_id"] != l.roomID || row["party_id"] != l.partyID {
		t.Errorf("the repair landed on %v", row)
	}
	if row["status"] != "OPEN" || row["priority"] != "URGENT" {
		t.Errorf("the schema's CHECK vocabulary was not written: %v", row)
	}

	list, err := l.repo.Tickets(ctx, tenancy)
	if err != nil || len(list) != 1 {
		t.Fatalf("Tickets = %+v, %v", list, err)
	}
}

// Reporting a payment, and the retry that must not become a second transfer.
func TestLiveAReportedPaymentIsRecordedOnceAndCountsForNothing(t *testing.T) {
	l := newLive(t)
	l.claim()
	l.billing()

	ctx := context.Background()
	tenancy, _ := l.repo.ResolveTenancy(ctx, l.accountID)

	before, _ := l.repo.Invoices(ctx, tenancy)

	for i := 0; i < 2; i++ {
		if err := l.repo.ReportPayment(ctx, tenancy, l.invoiceID, 325000,
			"promptpay", "REF-9911", "idem-live-1"); err != nil {
			t.Fatalf("ReportPayment attempt %d: %v", i+1, err)
		}
	}

	r := l.fake.Row(`SELECT COUNT(*) AS n FROM payment
	                 WHERE tenant_id = ? AND idempotency_key = 'idem-live-1'`, l.tenantID)
	if r["n"] != float64(1) {
		t.Errorf("the retry wrote %v rows", r["n"])
	}

	row := l.fake.Row(`SELECT status, reported_by_account, method FROM payment
	                   WHERE tenant_id = ? AND idempotency_key = 'idem-live-1'`, l.tenantID)
	if row["status"] != "REPORTED" || row["reported_by_account"] != l.accountID {
		t.Errorf("payment = %v", row)
	}

	after, _ := l.repo.Invoices(ctx, tenancy)
	if after[0].DueSatang != before[0].DueSatang {
		t.Errorf("an unverified report moved the balance from %d to %d",
			before[0].DueSatang, after[0].DueSatang)
	}
}

// Two operators, one person. `client_tenancy_mode` is MULTI for this vertical,
// and the reads must still answer for one operator only.
func TestLiveOneOperatorsRowsAreInvisibleToAnother(t *testing.T) {
	first := newLive(t)
	first.claim()
	first.billing()

	// A second operator, on the same database, with a lease for the same
	// person and an invoice of their own.
	other := ulid.New()
	otherRoom, otherParty, otherContract, otherBuilding := ulid.New(), ulid.New(), ulid.New(), ulid.New()
	ts := now()
	first.fake.Exec(`INSERT INTO tenant (tenant_id, slug, name, owner_account_id, status, vertical,
	                                     locale, timezone, currency, data_region, created_at, updated_at)
	                 VALUES (?, 'other-dorm', 'หออื่น', ?, 'ACTIVE', 'DORM',
	                         'th', 'Asia/Bangkok', 'THB', 'auto', ?, ?)`,
		other, first.accountID, ts, ts)
	first.fake.Exec(`INSERT INTO building (tenant_id, building_id, name, created_at, updated_at)
	                 VALUES (?, ?, 'อาคารอื่น', ?, ?)`, other, otherBuilding, ts, ts)
	first.fake.Exec(`INSERT INTO room (tenant_id, room_id, building_id, number, rent, status,
	                                   created_at, updated_at)
	                 VALUES (?, ?, ?, '101', 300000, 'OCCUPIED', ?, ?)`,
		other, otherRoom, otherBuilding, ts, ts)
	first.fake.Exec(`INSERT INTO party (tenant_id, party_id, kind, display_name, created_by,
	                                    created_at, updated_at)
	                 VALUES (?, ?, 'PRIMARY', 'ผู้เช่า ทดสอบ', 'STAFF', ?, ?)`,
		other, otherParty, ts, ts)
	first.fake.Exec(`INSERT INTO contract (tenant_id, contract_id, room_id, party_id, start_date,
	                                       rent, deposit, status, created_at, updated_at)
	                 VALUES (?, ?, ?, ?, '2026-01-01', 300000, 0, 'ACTIVE', ?, ?)`,
		other, otherContract, otherRoom, otherParty, ts, ts)
	otherInvoice := ulid.New()
	first.fake.Exec(`INSERT INTO invoice (tenant_id, invoice_id, number, building_id, room_id,
	                                      contract_id, party_id, period, issue_date, due_date,
	                                      subtotal, total, status, created_at, updated_at)
	                 VALUES (?, ?, 'INV-2026-09-0001', ?, ?, ?, ?, '2026-09', '2026-09-01',
	                         '2026-09-05', 300000, 300000, 'UNPAID', ?, ?)`,
		other, otherInvoice, otherBuilding, otherRoom, otherContract, otherParty, ts, ts)

	ctx := context.Background()
	tenancy, err := first.repo.ResolveTenancy(ctx, first.accountID)
	if err != nil {
		t.Fatalf("ResolveTenancy: %v", err)
	}

	invoices, err := first.repo.Invoices(ctx, tenancy)
	if err != nil {
		t.Fatalf("Invoices: %v", err)
	}
	if len(invoices) != 1 || invoices[0].ID != first.invoiceID {
		t.Fatalf("invoices = %+v, want only the resolved operator's", invoices)
	}

	// Naming the other operator's invoice is not found, not forbidden: a 403
	// would confirm the id is real.
	if _, err := first.repo.Invoice(ctx, tenancy, otherInvoice); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound for another operator's invoice", err)
	}
	if _, err := first.repo.PaymentInfo(ctx, tenancy, otherInvoice); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
	// Two operators may hold the same invoice number. The pre-XYZ index was
	// global and would have refused the row seeded above.
	if err := first.repo.ReportPayment(ctx, tenancy, otherInvoice, 1000,
		"promptpay", "", "idem-x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want a payment against another operator's invoice refused", err)
	}
}

// Sign-in, twice, against the real unique index on the identity.
func TestLiveSignInIsIdempotent(t *testing.T) {
	l := newLive(t)
	ctx := context.Background()

	first, err := l.repo.AccountByLine(ctx, "U_live_subject", "ผู้เช่า ทดสอบ")
	if err != nil {
		t.Fatalf("AccountByLine: %v", err)
	}
	second, err := l.repo.AccountByLine(ctx, "U_live_subject", "ผู้เช่า ทดสอบ")
	if err != nil {
		t.Fatalf("AccountByLine again: %v", err)
	}
	if first.ID != second.ID {
		t.Errorf("one person became two accounts: %s and %s", first.ID, second.ID)
	}

	r := l.fake.Row(`SELECT COUNT(*) AS n FROM account_identity WHERE provider = 'LINE'`)
	if r["n"] != float64(1) {
		t.Errorf("identities = %v, want 1", r["n"])
	}
}

// The whole recovery flow against real constraints: an address, a token spent
// once, and a rebind that moves the credential and nothing else.
func TestLiveRecoveryMovesOnlyTheCredential(t *testing.T) {
	l := newLive(t)
	l.claim()
	ctx := context.Background()

	if err := l.repo.SetEmail(ctx, l.accountID, "tenant@example.com"); err != nil {
		t.Fatalf("SetEmail: %v", err)
	}
	if _, err := l.repo.AccountByVerifiedEmail(ctx, "tenant@example.com"); !errors.Is(err, ErrNotFound) {
		t.Fatal("an unverified address was treated as evidence")
	}
	if err := l.repo.MarkEmailVerified(ctx, l.accountID, "tenant@example.com"); err != nil {
		t.Fatalf("MarkEmailVerified: %v", err)
	}
	found, err := l.repo.AccountByVerifiedEmail(ctx, "tenant@example.com")
	if err != nil || found != l.accountID {
		t.Fatalf("AccountByVerifiedEmail = %q, %v", found, err)
	}

	// The address is taken now, so another account cannot claim it.
	stranger := ulid.New()
	l.fake.Exec(`INSERT INTO account (account_id, display_name, locale, status, created_at, updated_at)
	             VALUES (?, 'คนอื่น', 'th', 'ACTIVE', ?, ?)`, stranger, now(), now())
	if err := l.repo.SetEmail(ctx, stranger, "tenant@example.com"); !errors.Is(err, ErrEmailTaken) {
		t.Errorf("err = %v, want ErrEmailTaken", err)
	}

	if err := l.repo.IssueAuthToken(ctx, "RECOVERY", l.accountID, "a-hash",
		"tenant@example.com", mustParse(t, "2099-01-01T00:00:00Z"), ""); err != nil {
		t.Fatalf("IssueAuthToken: %v", err)
	}
	who, sentTo, err := l.repo.ConsumeAuthToken(ctx, "RECOVERY", "a-hash")
	if err != nil || who != l.accountID || sentTo != "tenant@example.com" {
		t.Fatalf("ConsumeAuthToken = %q, %q, %v", who, sentTo, err)
	}
	if _, _, err := l.repo.ConsumeAuthToken(ctx, "RECOVERY", "a-hash"); !errors.Is(err, ErrNotFound) {
		t.Error("the same link was spent twice")
	}

	// Bind a LINE identity, then move it.
	if _, err := l.repo.AccountByLine(ctx, "U_old", "ผู้เช่า ทดสอบ"); err != nil {
		t.Fatalf("AccountByLine: %v", err)
	}
	lineAccount, _ := l.repo.AccountByLine(ctx, "U_old", "")
	if err := l.repo.RebindLine(ctx, lineAccount.ID, "U_old", "U_new", "req-live"); err != nil {
		t.Fatalf("RebindLine: %v", err)
	}
	moved, err := l.repo.LineSubjectFor(ctx, lineAccount.ID)
	if err != nil || moved != "U_new" {
		t.Fatalf("LineSubjectFor = %q, %v", moved, err)
	}

	// The resident's own lease is untouched.
	party := l.fake.Row(`SELECT account_id FROM party WHERE tenant_id = ? AND party_id = ?`,
		l.tenantID, l.partyID)
	if party["account_id"] != l.accountID {
		t.Errorf("a rebind moved the lease to %v", party["account_id"])
	}
}

func mustParse(t *testing.T, value string) time.Time {
	t.Helper()
	at, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatalf("parse %q: %v", value, err)
	}
	return at
}

// Inbound routing against the real schema: the idempotency table, the
// membership lookup a userId resolves through, and the conversation context's
// upsert.
func TestLiveInboundRoutesWithoutTrustingThePayload(t *testing.T) {
	l := newLive(t)
	l.claim()
	ctx := context.Background()

	// Give the account the LINE identity a webhook would arrive with.
	if _, err := l.repo.AccountByLine(ctx, "U_resident", "ผู้เช่า ทดสอบ"); err != nil {
		t.Fatalf("AccountByLine: %v", err)
	}
	lineAccount, _ := l.repo.AccountByLine(ctx, "U_resident", "")
	// The claim above bound the party to a different account, so move the
	// membership onto the one the webhook will resolve to.
	l.fake.Exec(`UPDATE membership SET account_id = ? WHERE tenant_id = ?`, lineAccount.ID, l.tenantID)

	accountID, operators, err := l.repo.Inbound(ctx, "U_resident")
	if err != nil {
		t.Fatalf("Inbound: %v", err)
	}
	if accountID != lineAccount.ID {
		t.Errorf("account = %q", accountID)
	}
	if len(operators) != 1 || operators[0].TenantID != l.tenantID {
		t.Fatalf("operators = %+v", operators)
	}
	if operators[0].Name != "หอพักทดสอบ" {
		t.Errorf("operator = %+v", operators[0])
	}

	// A tenant the account is not a member of is not found, not forbidden.
	if _, err := l.repo.ResolveTenancyIn(ctx, accountID, ulid.New()); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound for a tenant nobody is in", err)
	}

	// LINE redelivers. The second sighting of one event is not fresh.
	fresh, err := l.repo.MarkWebhookEvent(ctx, "LINE", "01EVENT")
	if err != nil || !fresh {
		t.Fatalf("first delivery: fresh=%v err=%v", fresh, err)
	}
	fresh, err = l.repo.MarkWebhookEvent(ctx, "LINE", "01EVENT")
	if err != nil || fresh {
		t.Fatalf("redelivery: fresh=%v err=%v", fresh, err)
	}

	// The upsert, twice, because the second message in a conversation slides
	// the window rather than failing on the primary key.
	for i := 0; i < 2; i++ {
		if err := l.repo.RememberConversationTenant(ctx, accountID, ChannelLINE, l.tenantID); err != nil {
			t.Fatalf("RememberConversationTenant %d: %v", i+1, err)
		}
	}
	if r := l.fake.Row(`SELECT COUNT(*) AS n FROM conversation_context WHERE account_id = ?`, accountID); r["n"] != float64(1) {
		t.Errorf("conversation rows = %v, want 1", r["n"])
	}
	got, err := l.repo.ConversationTenant(ctx, accountID, ChannelLINE)
	if err != nil || got != l.tenantID {
		t.Fatalf("ConversationTenant = %q, %v", got, err)
	}

	// INV-34's safety net: the context stops resolving once the membership is
	// no longer one that occupies, even if the row outlives it.
	l.fake.Exec(`UPDATE membership SET status = 'LEFT' WHERE tenant_id = ?`, l.tenantID)
	if _, err := l.repo.ConversationTenant(ctx, accountID, ChannelLINE); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want a context pointing at a membership that ended to stop resolving", err)
	}

	// And the same account now has nothing to route to.
	_, operators, err = l.repo.Inbound(ctx, "U_resident")
	if err != nil {
		t.Fatalf("Inbound: %v", err)
	}
	if len(operators) != 0 {
		t.Errorf("operators = %+v, want none after LEFT", operators)
	}
}

// The office details behind the rich menu's ADMIN button.
func TestLiveContactReadsTheCallersOwnBuilding(t *testing.T) {
	l := newLive(t)
	l.claim()
	l.fake.Exec(`UPDATE building SET address = '123 ถนนทดสอบ' WHERE tenant_id = ?`, l.tenantID)

	ctx := context.Background()
	tenancy, err := l.repo.ResolveTenancy(ctx, l.accountID)
	if err != nil {
		t.Fatalf("ResolveTenancy: %v", err)
	}
	contact, err := l.repo.Contact(ctx, tenancy)
	if err != nil {
		t.Fatalf("Contact: %v", err)
	}
	if contact.OperatorName != "หอพักทดสอบ" || contact.BuildingName != "Oscar Apartment" {
		t.Errorf("contact = %+v", contact)
	}
	if contact.Address != "123 ถนนทดสอบ" || contact.RoomNumber != "609" {
		t.Errorf("contact = %+v", contact)
	}
}
