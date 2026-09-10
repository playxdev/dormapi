package httpx

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/playxdev/dormapi/internal/auth"
	"github.com/playxdev/dormapi/internal/d1/d1test"
)

// tenancyRow is what ResolveTenancy answers with, so requireTenancy succeeds
// and the handler under test is actually reached.
func tenancyRow() map[string]any {
	return map[string]any{
		"tenant_id": "01TENANT", "membership_id": "01MEMBER", "operator_name": "หอพักทดสอบ",
		"party_id": "01PARTY", "account_id": "01ACCOUNT", "display_name": "ผู้เช่า ทดสอบ",
		"email": "tenant@example.com", "verified_at": "2026-09-01T00:00:00.000Z",
		"contract_id": "01CONTRACT", "building_id": "01BUILDING", "building_name": "Oscar Apartment",
		"room_id": "01ROOM", "room_number": "609",
	}
}

func session(t *testing.T, api *API, accountID string) string {
	t.Helper()
	token, _, err := api.Issuer.Issue(accountID)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	return token
}

func get(t *testing.T, api *API, path, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	api.Routes([]string{"https://dorm.playxdev.com"}).ServeHTTP(rec, req)
	return rec
}

// Everything under /me needs a session. A missing or forged token is 401, and
// nothing reaches the database.
func TestEveryTenantRouteRequiresASession(t *testing.T) {
	paths := []string{
		"/api/v1/me", "/api/v1/me/invoices", "/api/v1/me/invoices/01INV",
		"/api/v1/me/invoices/01INV/payment", "/api/v1/me/repairs",
		"/api/v1/me/repairs/01TICKET", "/api/v1/me/announcements",
		"/api/v1/me/announcements/01ANN", "/api/v1/me/meters",
		"/api/v1/invites/A7K9Q2MX",
	}
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			for name, token := range map[string]string{
				"no token":      "",
				"a forged one":  "not-a-jwt",
				"another key's": mustIssue(t, "some-other-signing-secret-at-least-32b"),
			} {
				fake := d1test.New(t)
				api := newAPI(t, fake, &stubVerifier{}, &recordingMail{})

				rec := get(t, api, path, token)
				if rec.Code != http.StatusUnauthorized {
					t.Errorf("%s: status = %d, want 401", name, rec.Code)
				}
				if len(fake.Calls) != 0 {
					t.Errorf("%s: reached the database unauthenticated: %#v", name, fake.Calls)
				}
			}
		})
	}
}

func mustIssue(t *testing.T, secret string) string {
	t.Helper()
	token, _, err := auth.NewIssuer([]byte(secret), 0).Issue("01ACCOUNT")
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	return token
}

// Signed in, no room yet. The MINI App shows "your account is not linked to a
// dormitory", so this has to be a code it can tell apart from a real failure.
func TestTenantRoutesAnswerTenancyNotFound(t *testing.T) {
	for _, path := range []string{
		"/api/v1/me", "/api/v1/me/invoices", "/api/v1/me/meters",
		"/api/v1/me/repairs", "/api/v1/me/announcements",
	} {
		t.Run(path, func(t *testing.T) {
			fake := d1test.New(t, d1test.Answer{}) // no membership
			api := newAPI(t, fake, &stubVerifier{}, &recordingMail{})

			rec := get(t, api, path, session(t, api, "01ACCOUNT"))
			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404", rec.Code)
			}
			if !strings.Contains(rec.Body.String(), "tenancy_not_found") {
				t.Errorf("body = %q", rec.Body.String())
			}
			// Resolving is one round trip, and it is the only one: nothing
			// downstream runs without a tenant.
			if len(fake.Calls) != 1 {
				t.Errorf("ran %d statements, want to stop at the resolve", len(fake.Calls))
			}
		})
	}
}

// The membership is resolved once per request, not once per statement.
func TestMeAnswersFromTheResolvedMembership(t *testing.T) {
	fake := d1test.New(t, d1test.Answer{Rows: []map[string]any{tenancyRow()}})
	api := newAPI(t, fake, &stubVerifier{}, &recordingMail{})

	rec := get(t, api, "/api/v1/me", session(t, api, "01ACCOUNT"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := map[string]any{
		"user_id": "01ACCOUNT", "resident_id": "01PARTY",
		"operator_name": "หอพักทดสอบ", "contract_id": "01CONTRACT",
		"property_id": "01BUILDING", "property_name": "Oscar Apartment",
		"room_id": "609", "display_name": "ผู้เช่า ทดสอบ",
		"email": "tenant@example.com", "email_verified": true,
	}
	for key, value := range want {
		if body[key] != value {
			t.Errorf("%s = %#v, want %#v", key, body[key], value)
		}
	}

	// The old name is gone. Under XYZ a tenant is the business, and leaving
	// the field would have it mean two things at once.
	if _, ok := body["tenant_id"]; ok {
		t.Error("tenant_id is still on the wire")
	}
	if len(fake.Calls) != 1 {
		t.Errorf("ran %d statements for /me, want 1", len(fake.Calls))
	}
}

// A row that is not the caller's is the same answer as a row that does not
// exist. A 403 would confirm the id is real.
func TestNamingSomebodyElsesRowIsNotFound(t *testing.T) {
	cases := map[string]struct {
		path string
		code string
	}{
		"an invoice":      {"/api/v1/me/invoices/01SOMEONEELSES", "invoice_not_found"},
		"a payment QR":    {"/api/v1/me/invoices/01SOMEONEELSES/payment", "invoice_not_found"},
		"a repair":        {"/api/v1/me/repairs/01SOMEONEELSES", "repair_not_found"},
		"an announcement": {"/api/v1/me/announcements/01SOMEONEELSES", "announcement_not_found"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			fake := d1test.New(t,
				d1test.Answer{Rows: []map[string]any{tenancyRow()}},
				d1test.Answer{}, // the row is not this tenant's
			)
			api := newAPI(t, fake, &stubVerifier{}, &recordingMail{})

			rec := get(t, api, c.path, session(t, api, "01ACCOUNT"))
			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404", rec.Code)
			}
			if !strings.Contains(rec.Body.String(), c.code) {
				t.Errorf("body = %q, want %s", rec.Body.String(), c.code)
			}
			if rec.Code == http.StatusForbidden {
				t.Error("403 confirms the row exists")
			}
		})
	}
}

// The outstanding total is summed from the list already in hand rather than
// asked of the database, and a credit on one invoice does not cancel a debt on
// another.
func TestInvoiceListCarriesTheOutstandingTotal(t *testing.T) {
	fake := d1test.New(t,
		d1test.Answer{Rows: []map[string]any{tenancyRow()}},
		d1test.Answer{Rows: []map[string]any{
			{"invoice_id": "01A", "number": "INV-1", "period": "2026-09", "status": "UNPAID",
				"due_date": "2026-09-05", "total": float64(525000), "paid": float64(200000)},
			{"invoice_id": "01B", "number": "INV-2", "period": "2026-08", "status": "UNPAID",
				"due_date": "2026-08-05", "total": float64(500000), "paid": float64(500000)},
			{"invoice_id": "01C", "number": "INV-3", "period": "2026-07", "status": "UNPAID",
				"due_date": "2026-07-05", "total": float64(500000), "paid": float64(600000)},
		}},
	)
	api := newAPI(t, fake, &stubVerifier{}, &recordingMail{})

	rec := get(t, api, "/api/v1/me/invoices", session(t, api, "01ACCOUNT"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}

	var body struct {
		Invoices []struct {
			Status    string `json:"status"`
			DueSatang int64  `json:"due_satang"`
		} `json:"invoices"`
		Outstanding int64 `json:"outstanding_satang"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Invoices) != 3 {
		t.Fatalf("got %d invoices", len(body.Invoices))
	}
	// Only the 325000 still owed. The settled one adds nothing and the
	// overpaid one does not subtract.
	if body.Outstanding != 325000 {
		t.Errorf("outstanding = %d, want 325000", body.Outstanding)
	}
	if body.Invoices[0].Status != "partial" || body.Invoices[1].Status != "paid" {
		t.Errorf("statuses = %+v", body.Invoices)
	}
}

// Absence of a read receipt is what unread means, so the count is derived from
// the list rather than from a second round trip.
func TestAnnouncementListCountsTheUnread(t *testing.T) {
	fake := d1test.New(t,
		d1test.Answer{Rows: []map[string]any{tenancyRow()}},
		d1test.Answer{Rows: []map[string]any{
			{"announcement_id": "01A", "title": "น้ำหยุดไหล", "body": "…",
				"pinned": float64(1), "is_read": float64(0), "published_at": "2026-09-04"},
			{"announcement_id": "01B", "title": "เก็บขยะ", "body": "…",
				"pinned": float64(0), "is_read": float64(1), "published_at": "2026-08-20"},
		}},
	)
	api := newAPI(t, fake, &stubVerifier{}, &recordingMail{})

	rec := get(t, api, "/api/v1/me/announcements", session(t, api, "01ACCOUNT"))
	var body struct {
		Unread int `json:"unread_count"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body.Unread != 1 {
		t.Errorf("unread = %d, want 1", body.Unread)
	}
	if len(fake.Calls) != 2 {
		t.Errorf("ran %d statements, want the resolve and the list", len(fake.Calls))
	}
}

// Setting an address and reviewing an invitation are exactly what an account
// with no room does, so neither may sit behind the tenancy check.
func TestOnboardingRoutesDoNotRequireARoom(t *testing.T) {
	fake := d1test.New(t, d1test.Answer{}) // the code resolves to nothing
	api := newAPI(t, fake, &stubVerifier{}, &recordingMail{})

	rec := get(t, api, "/api/v1/invites/A7K9Q2MX", session(t, api, "01ACCOUNT"))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	// invite_not_found, not tenancy_not_found: the caller was never asked to
	// have a room.
	if !strings.Contains(rec.Body.String(), "invite_not_found") {
		t.Errorf("body = %q", rec.Body.String())
	}
	if len(fake.Calls) != 1 {
		t.Fatalf("ran %d statements: %#v", len(fake.Calls), fake.Calls)
	}
	if strings.Contains(fake.Calls[0].SQL, "FROM membership") {
		t.Error("reviewing an invitation resolved a tenancy first")
	}
}

// Reporting a payment is a claim about money this system cannot see. It is
// accepted for verification, never as settled.
func TestReportingAPaymentIsAcceptedNotConfirmed(t *testing.T) {
	fake := d1test.New(t,
		d1test.Answer{Rows: []map[string]any{tenancyRow()}},
		d1test.Answer{Changes: 1},
	)
	api := newAPI(t, fake, &stubVerifier{}, &recordingMail{})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/me/invoices/01INV/payments",
		strings.NewReader(`{"amount_satang":325000,"method":"promptpay","idempotency_key":"k1"}`))
	req.Header.Set("Authorization", "Bearer "+session(t, api, "01ACCOUNT"))
	rec := httptest.NewRecorder()
	api.Routes([]string{"https://dorm.playxdev.com"}).ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "pending_verification") {
		t.Errorf("body = %q", rec.Body.String())
	}
}

func TestABadBodyIsRefusedBeforeTheDatabase(t *testing.T) {
	for _, c := range []struct{ name, path, body string }{
		{"a repair with no title", "/api/v1/me/repairs", `{"title":"  "}`},
		{"a payment with no amount", "/api/v1/me/invoices/01INV/payments",
			`{"amount_satang":0,"idempotency_key":"k"}`},
		{"not json at all", "/api/v1/me/repairs", `{`},
	} {
		t.Run(c.name, func(t *testing.T) {
			fake := d1test.New(t, d1test.Answer{Rows: []map[string]any{tenancyRow()}})
			api := newAPI(t, fake, &stubVerifier{}, &recordingMail{})

			req := httptest.NewRequest(http.MethodPost, c.path, strings.NewReader(c.body))
			req.Header.Set("Authorization", "Bearer "+session(t, api, "01ACCOUNT"))
			rec := httptest.NewRecorder()
			api.Routes([]string{"https://dorm.playxdev.com"}).ServeHTTP(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
			}
			// Only the tenancy resolve, which the middleware ran before the
			// handler saw the body.
			if len(fake.Calls) != 1 {
				t.Errorf("bad input reached the database: %#v", fake.Calls[1:])
			}
		})
	}
}

// The health check exists because the service once ran for days on a database
// id that had been deleted, reporting healthy the whole time.
func TestHealthReflectsTheDatabase(t *testing.T) {
	fake := d1test.New(t, d1test.Answer{Rows: []map[string]any{{"1": float64(1)}}})
	api := newAPI(t, fake, &stubVerifier{}, &recordingMail{})
	if rec := get(t, api, "/healthz", ""); rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}

	fake = d1test.New(t, d1test.Answer{Failure: "no such database"})
	api = newAPI(t, fake, &stubVerifier{}, &recordingMail{})
	rec := get(t, api, "/healthz", "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
	// The error text can name the account and the database.
	if strings.Contains(rec.Body.String(), "no such database") {
		t.Errorf("the database error reached the caller: %q", rec.Body.String())
	}
}
