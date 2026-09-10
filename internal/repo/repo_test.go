package repo

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/playxdev/dormapi/internal/d1/d1test"
	"github.com/playxdev/dormapi/internal/pii"
)

// The pepper is the development one from `backoffice/.dev.vars.example`, so a
// hash asserted here is a hash the backoffice would have written.
const testPepper = "ZGV2LW9ubHktcGlpLXBlcHBlci0zMi1ieXRlcyEhISE="

const (
	testTenant   = "01TENANT0000000000000000AA"
	testParty    = "01PARTY00000000000000000AA"
	testAccount  = "01ACCOUNT0000000000000000A"
	testContract = "01CONTRACT000000000000000A"
)

type harness struct {
	t    *testing.T
	fake *d1test.Server
	repo *Repo
}

func newHarness(t *testing.T, answers ...d1test.Answer) *harness {
	t.Helper()
	pepper, err := pii.ParsePepper(testPepper)
	if err != nil {
		t.Fatalf("parse pepper: %v", err)
	}
	fake := d1test.New(t, answers...)
	return &harness{
		t:    t,
		fake: fake,
		repo: New(fake.Client(), pepper, "2011358311", "https://backoffice.example"),
	}
}

// tenancy is what ResolveTenancy would have produced. Building it here rather
// than in a handler is the point of the unexported fields: outside this
// package there is no way to make one at all.
func (h *harness) tenancy() *Tenancy {
	return &Tenancy{
		tenantID:     testTenant,
		membershipID: "01MEMBER00000000000000000A",
		partyID:      testParty,
		Account:      Account{ID: testAccount, Name: "ผู้เช่า ทดสอบ"},
		ResidentID:   testParty,
		ContractID:   testContract,
	}
}

func (h *harness) only() d1test.Call {
	h.t.Helper()
	if len(h.fake.Calls) != 1 {
		h.t.Fatalf("ran %d statements, want 1: %#v", len(h.fake.Calls), h.fake.Calls)
	}
	return h.fake.Calls[0]
}

// requireSQL fails unless every fragment appears in the statement. Fragments,
// not the whole text, so reformatting the SQL does not break the test while
// dropping a guard from it still does.
func requireSQL(t *testing.T, sql string, fragments ...string) {
	t.Helper()
	normalised := strings.Join(strings.Fields(sql), " ")
	for _, want := range fragments {
		if !strings.Contains(normalised, strings.Join(strings.Fields(want), " ")) {
			t.Errorf("statement is missing %q\ngot: %s", want, normalised)
		}
	}
}

func hasParam(params []any, want any) bool {
	for _, p := range params {
		if p == want {
			return true
		}
	}
	return false
}

// Rule 3, and the reason this package exists: D1 has no row-level security, so
// a statement that forgets its tenant is caught by nothing at runtime. Every
// read a resident can reach is checked here in one place, because the failure
// is silent and the blast radius is another operator's data.
func TestEveryScopedReadCarriesTheResolvedTenant(t *testing.T) {
	cases := []struct {
		name string
		run  func(h *harness) error
	}{
		{"invoices", func(h *harness) error {
			_, err := h.repo.Invoices(context.Background(), h.tenancy())
			return err
		}},
		{"invoice", func(h *harness) error {
			_, err := h.repo.Invoice(context.Background(), h.tenancy(), "01INV")
			return err
		}},
		{"meters", func(h *harness) error {
			_, err := h.repo.Meters(context.Background(), h.tenancy())
			return err
		}},
		{"tickets", func(h *harness) error {
			_, err := h.repo.Tickets(context.Background(), h.tenancy())
			return err
		}},
		{"ticket", func(h *harness) error {
			_, err := h.repo.Ticket(context.Background(), h.tenancy(), "01TICKET")
			return err
		}},
		{"announcements", func(h *harness) error {
			_, err := h.repo.Announcements(context.Background(), h.tenancy())
			return err
		}},
		{"announcement", func(h *harness) error {
			_, err := h.repo.Announcement(context.Background(), h.tenancy(), "01ANN")
			return err
		}},
		{"payment info", func(h *harness) error {
			_, err := h.repo.PaymentInfo(context.Background(), h.tenancy(), "01INV")
			return err
		}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			_ = c.run(h)

			if len(h.fake.Calls) == 0 {
				t.Fatal("ran no statement at all")
			}
			for i, call := range h.fake.Calls {
				requireSQL(t, call.SQL, "tenant_id = ?1")
				if call.Params[0] != testTenant {
					t.Errorf("statement %d binds %#v as the tenant, want the resolved one",
						i, call.Params[0])
				}
			}
		})
	}
}

// The resident's own row is reached through the party the membership resolved
// to, never through an id that travelled with the request.
func TestReadsAreBoundedByTheCallersOwnParty(t *testing.T) {
	for _, c := range []struct {
		name string
		run  func(h *harness)
	}{
		{"invoices", func(h *harness) { h.repo.Invoices(context.Background(), h.tenancy()) }},
		{"meters", func(h *harness) { h.repo.Meters(context.Background(), h.tenancy()) }},
		{"tickets", func(h *harness) { h.repo.Tickets(context.Background(), h.tenancy()) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			c.run(h)
			if !hasParam(h.only().Params, testParty) {
				t.Errorf("no party in the statement: %#v", h.only().Params)
			}
		})
	}
}

// A reading belongs to a room and a room outlives a tenancy. The contract is
// what separates this resident's consumption from the previous occupant's.
func TestMetersAreBoundedByTheLeaseNotTheRoom(t *testing.T) {
	h := newHarness(t)
	if _, err := h.repo.Meters(context.Background(), h.tenancy()); err != nil {
		t.Fatalf("Meters: %v", err)
	}
	requireSQL(t, h.only().SQL,
		"JOIN contract c ON c.tenant_id = m.tenant_id AND c.contract_id = m.contract_id",
		"c.party_id = ?2",
		"m.status IN ('RECORDED', 'CONFIRMED')",
	)
}

// A draft has not been published and an expired notice is off the board.
// Excluded in the query rather than in Go, so a caller that forgets a
// condition gets no rows instead of somebody's unpublished draft.
func TestAnnouncementsExcludeDraftsAndExpired(t *testing.T) {
	h := newHarness(t)
	if _, err := h.repo.Announcements(context.Background(), h.tenancy()); err != nil {
		t.Fatalf("Announcements: %v", err)
	}
	requireSQL(t, h.only().SQL,
		"a.published_at IS NOT NULL",
		"(a.expires_at IS NULL OR a.expires_at >= ?4)",
		"c.status IN ('ACTIVE', 'ENDING')",
	)
}

// Only a verified payment counts. A reported one has been submitted and not
// yet accepted; showing it as settled tells the resident they owe nothing
// while the operator still thinks otherwise.
func TestABalanceCountsVerifiedPaymentsOnly(t *testing.T) {
	h := newHarness(t)
	if _, err := h.repo.Invoices(context.Background(), h.tenancy()); err != nil {
		t.Fatalf("Invoices: %v", err)
	}
	requireSQL(t, h.only().SQL,
		"pm.status = 'VERIFIED'",
		"i.status NOT IN ('DRAFT', 'VOID')",
	)
}

func TestEffectiveStatus(t *testing.T) {
	const today = "2026-09-10"
	cases := []struct {
		name                string
		status              string
		total, paid         int64
		dueDate, wantStatus string
	}{
		{"void stays void", "VOID", 100, 0, "2026-09-05", "VOID"},
		{"a draft stays a draft", "DRAFT", 100, 0, "2026-09-05", "DRAFT"},
		{"settled in full", "UNPAID", 100, 100, "2026-09-30", "PAID"},
		{"overpaid is still paid", "UNPAID", 100, 150, "2026-09-30", "PAID"},
		{"part paid", "UNPAID", 100, 40, "2026-09-30", "PARTIAL"},
		{"part paid and late is still partial", "UNPAID", 100, 40, "2026-09-05", "PARTIAL"},
		{"nothing paid, still in time", "UNPAID", 100, 0, "2026-09-30", "UNPAID"},
		{"nothing paid, past due", "UNPAID", 100, 0, "2026-09-05", "OVERDUE"},
		{"due today is not yet overdue", "UNPAID", 100, 0, today, "UNPAID"},
		// A zero-total invoice is not "paid" by paying nothing; it has no
		// money in it, and calling it PAID would put a green tick against an
		// invoice nobody settled.
		{"zero total", "UNPAID", 0, 0, "2026-09-30", "UNPAID"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := effectiveStatus(c.status, c.total, c.paid, c.dueDate, today)
			if got != c.wantStatus {
				t.Errorf("effectiveStatus = %q, want %q", got, c.wantStatus)
			}
		})
	}
}

// A report is a claim about money this system cannot see. It is written
// REPORTED and counts for nothing until the operator matches it to a
// statement.
func TestReportedPaymentDoesNotCountItself(t *testing.T) {
	h := newHarness(t, d1test.Answer{Changes: 1})
	err := h.repo.ReportPayment(context.Background(), h.tenancy(), "01INV",
		450000, "promptpay", "REF123", "idem-1")
	if err != nil {
		t.Fatalf("ReportPayment: %v", err)
	}

	c := h.only()
	requireSQL(t, c.SQL, "INSERT INTO payment", "'REPORTED'", "i.party_id = ?3")
	if strings.Contains(c.SQL, "'VERIFIED'") {
		t.Error("a resident's report writes a verified payment")
	}
	if !hasParam(c.Params, "PROMPTPAY") {
		t.Errorf("method not translated to the schema's vocabulary: %#v", c.Params)
	}
	if !hasParam(c.Params, "idem-1") {
		t.Error("the idempotency key never reached the statement")
	}
}

// A resident on a bad connection taps twice. Two rows would look like two
// transfers, and the operator would be asked to verify money that arrived
// once.
func TestARetriedReportIsTheSamePayment(t *testing.T) {
	h := newHarness(t, d1test.Answer{Failure: "UNIQUE constraint failed: payment.idempotency_key"})
	if err := h.repo.ReportPayment(context.Background(), h.tenancy(), "01INV",
		450000, "promptpay", "", "idem-1"); err != nil {
		t.Fatalf("a retry was reported as a failure: %v", err)
	}
}

func TestReportPaymentValidatesItsInput(t *testing.T) {
	for _, c := range []struct {
		name           string
		amount         int64
		method, idem   string
		ref            string
		wantSQLCallsIs int
	}{
		{name: "no amount", amount: 0, method: "promptpay", idem: "k"},
		{name: "negative amount", amount: -1, method: "promptpay", idem: "k"},
		{name: "a method the operator cannot receive", amount: 1, method: "bitcoin", idem: "k"},
		{name: "no idempotency key", amount: 1, method: "promptpay", idem: " "},
		{name: "an oversized reference", amount: 1, method: "promptpay", idem: "k",
			ref: strings.Repeat("ก", 65)},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			err := h.repo.ReportPayment(context.Background(), h.tenancy(), "01INV",
				c.amount, c.method, c.ref, c.idem)
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("err = %v, want ErrInvalid", err)
			}
			if len(h.fake.Calls) != 0 {
				t.Errorf("bad input reached the database: %#v", h.fake.Calls)
			}
		})
	}
}

// Nothing about where a repair is filed comes from the request body: the room
// is read from the caller's own live lease inside the INSERT.
func TestCreateTicketTakesItsRoomFromTheLease(t *testing.T) {
	h := newHarness(t,
		d1test.Answer{Changes: 1},
		d1test.Answer{Rows: []map[string]any{{
			"ticket_id": "01TICKET", "title": "ไฟไม่ติด",
			"priority": "URGENT", "status": "OPEN",
		}}},
	)
	ticket, err := h.repo.CreateTicket(context.Background(), h.tenancy(), "ไฟไม่ติด", "", "urgent")
	if err != nil {
		t.Fatalf("CreateTicket: %v", err)
	}

	insert := h.fake.Calls[0]
	requireSQL(t, insert.SQL,
		"INSERT INTO ticket",
		"SELECT ?1, ?2, c.room_id, ?3",
		"FROM contract c",
		"c.status IN ('ACTIVE', 'ENDING')",
	)
	if !hasParam(insert.Params, "URGENT") {
		t.Errorf("priority not translated for the schema: %#v", insert.Params)
	}

	// The wire vocabulary is lowercase; the deployed app matches on it.
	if ticket.Status != "open" || ticket.Priority != "urgent" {
		t.Errorf("ticket = %+v, want lowercase status and priority", ticket)
	}
}

func TestCreateTicketValidatesItsInput(t *testing.T) {
	for _, c := range []struct{ name, title, detail, priority string }{
		{name: "no title", title: "  "},
		{name: "an oversized title", title: strings.Repeat("ก", 121)},
		{name: "an oversized detail", title: "ไฟไม่ติด", detail: strings.Repeat("ก", 2001)},
		{name: "a priority the operator does not filter on", title: "ไฟไม่ติด", priority: "asap"},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			_, err := h.repo.CreateTicket(context.Background(), h.tenancy(), c.title, c.detail, c.priority)
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("err = %v, want ErrInvalid", err)
			}
			if len(h.fake.Calls) != 0 {
				t.Errorf("bad input reached the database: %#v", h.fake.Calls)
			}
		})
	}
}

// Both services write to the same columns and both compare them as text. The
// backoffice writes JavaScript's toISOString; anything else here sorts against
// it wrongly.
func TestTimestampsMatchTheFormatTheBackofficeWrites(t *testing.T) {
	shape := regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}Z$`)
	if got := now(); !shape.MatchString(got) {
		t.Errorf("now() = %q, want the shape of new Date().toISOString()", got)
	}

	// Fixed width, so the ordering is the ordering of the instants. Trailing
	// zeros trimmed — as RFC3339Nano would — breaks that.
	at := timestamp(time.Date(2026, 9, 10, 16, 45, 12, 500_000_000, time.UTC))
	if at != "2026-09-10T16:45:12.500Z" {
		t.Errorf("timestamp = %q, want the fractional digits kept", at)
	}

	earlier := timestamp(time.Date(2026, 9, 10, 16, 45, 12, 631_000_000, time.UTC))
	later := timestamp(time.Date(2026, 9, 10, 16, 45, 12, 631_827_377, time.UTC))
	if earlier > later {
		t.Errorf("%q sorts after %q", earlier, later)
	}
}

// The operator's day, not UTC's, and not the host's.
//
// Between midnight and 07:00 in Bangkok the UTC date is still yesterday. An
// invoice due today would read as not yet due for another seven hours, and the
// backoffice — computing the same thing from the same rule — would disagree
// with the resident's app for exactly that window.
func TestTodayIsTheOperatorsDay(t *testing.T) {
	if bangkok.String() != "Asia/Bangkok" {
		t.Errorf("zone = %q, want Asia/Bangkok from the embedded tzdata", bangkok)
	}

	// The one moment that separates the two answers: 23:00 UTC is already
	// tomorrow in Bangkok.
	at := time.Date(2026, 9, 10, 23, 0, 0, 0, time.UTC)
	if got := at.In(bangkok).Format("2006-01-02"); got != "2026-09-11" {
		t.Errorf("date = %q, want the Bangkok day", got)
	}
	if got := at.UTC().Format("2006-01-02"); got == "2026-09-11" {
		t.Fatal("the test moment does not actually straddle midnight")
	}

	// Thailand has been UTC+7 with no daylight saving since 1941, so the
	// offset is the same in every month.
	for _, month := range []time.Month{time.January, time.July} {
		_, offset := time.Date(2026, month, 15, 12, 0, 0, 0, time.UTC).In(bangkok).Zone()
		if offset != 7*60*60 {
			t.Errorf("%s offset = %d, want +7h", month, offset)
		}
	}

	// And the running answer is that day, whatever the host is set to.
	if got, want := today(), time.Now().In(bangkok).Format("2006-01-02"); got != want {
		t.Errorf("today() = %q, want %q", got, want)
	}
}
