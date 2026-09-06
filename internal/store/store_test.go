package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/playxdev/dormapi/internal/d1/d1test"
)

// harness is a store wired to a fake D1, plus the statements it sent.
type harness struct {
	t     *testing.T
	fake  *d1test.Server
	store *Store
}

func newHarness(t *testing.T, answers ...d1test.Answer) *harness {
	fake := d1test.New(t, answers...)
	return &harness{t: t, fake: fake, store: New(fake.Client(), "https://backoffice.example")}
}

// only returns the single statement the test expected to be run.
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

func TestClaimInviteGuards(t *testing.T) {
	// An invite is single use, expires, and can be revoked. All three are
	// enforced in the WHERE clause rather than by reading first and writing
	// after, because D1 offers no transaction to hold between the two.
	h := newHarness(t, d1test.Answer{Changes: 1})
	err := h.store.ClaimInvite(context.Background(), "user-1", "K7M9P4QX", "1.0", "1.0")
	if err != nil {
		t.Fatalf("ClaimInvite: %v", err)
	}

	c := h.only()
	requireSQL(t, c.SQL,
		"UPDATE contracts SET",
		"WHERE confirmed_by_user_id IS NULL",
		"AND status = 'active'",
		"i.revoked_at IS NULL",
		"i.expires_at > datetime('now')",
	)

	// The agreed terms are recorded with the claim itself. Written by a second
	// statement they could be lost while the room stayed bound, leaving a
	// tenancy nobody can say the terms of.
	requireSQL(t, c.SQL, "agreed_terms_version = ?3", "agreed_pdpa_version = ?4")

	want := []any{"user-1", "K7M9P4QX", "1.0", "1.0"}
	if len(c.Params) != len(want) {
		t.Fatalf("params = %#v, want %#v", c.Params, want)
	}
	for i := range want {
		if c.Params[i] != want[i] {
			t.Errorf("param %d = %#v, want %#v", i+1, c.Params[i], want[i])
		}
	}
}

func TestClaimInviteAlreadyClaimedIsDistinct(t *testing.T) {
	// Nothing updated, and the preview says the contract already has a tenant.
	// The app says "this room is linked to another account" rather than "this
	// code does not work", which are different problems for the tenant.
	h := newHarness(t,
		d1test.Answer{Changes: 0},
		d1test.Answer{Rows: []map[string]any{{
			"code":                 "K7M9P4QX",
			"building_name":        "Oscar",
			"room_number":          "A-203",
			"tenant_name":          "หอมนภา",
			"rent":                 450000,
			"deposit":              900000,
			"start_date":           "2026-10-01",
			"confirmed_by_user_id": "someone-else",
		}}},
	)
	err := h.store.ClaimInvite(context.Background(), "user-1", "K7M9P4QX", "1.0", "1.0")
	if !errors.Is(err, ErrAlreadyClaimed) {
		t.Fatalf("ClaimInvite = %v, want ErrAlreadyClaimed", err)
	}
}

func TestClaimInviteUnusableCodeIsNotFound(t *testing.T) {
	// Unknown, expired and revoked codes are one answer on purpose: telling
	// them apart tells a stranger which codes exist.
	h := newHarness(t, d1test.Answer{Changes: 0}, d1test.Answer{})
	err := h.store.ClaimInvite(context.Background(), "user-1", "K7M9P4QX", "1.0", "1.0")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("ClaimInvite = %v, want ErrNotFound", err)
	}
}

func TestConsumeAuthTokenIsSingleUse(t *testing.T) {
	h := newHarness(t, d1test.Answer{Rows: []map[string]any{{"user_id": "user-1"}}})
	got, err := h.store.ConsumeAuthToken(context.Background(), "recovery", "hash")
	if err != nil {
		t.Fatalf("ConsumeAuthToken: %v", err)
	}
	if got != "user-1" {
		t.Errorf("user = %q, want user-1", got)
	}

	// Spending and reading are one statement. Two requests racing the same
	// link must not both come back with a user: the update matches only while
	// consumed_at is NULL, and RETURNING hands back the row it changed.
	c := h.only()
	requireSQL(t, c.SQL,
		"UPDATE auth_tokens SET consumed_at = datetime('now')",
		"consumed_at IS NULL",
		"expires_at > datetime('now')",
		"purpose = ?2",
		"RETURNING user_id",
	)
}

func TestConsumeAuthTokenSecondAttemptFails(t *testing.T) {
	// The row exists but was already consumed, so the guarded update matches
	// nothing. Indistinguishable from expired, which is what it now is.
	h := newHarness(t, d1test.Answer{})
	_, err := h.store.ConsumeAuthToken(context.Background(), "recovery", "hash")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("ConsumeAuthToken = %v, want ErrNotFound", err)
	}
}

func TestSetEmailRejectsAnAddressOnAnotherAccount(t *testing.T) {
	h := newHarness(t, d1test.Answer{Changes: 0})
	err := h.store.SetEmail(context.Background(), "user-1", "Taken@Example.com")
	if !errors.Is(err, ErrEmailTaken) {
		t.Fatalf("SetEmail = %v, want ErrEmailTaken", err)
	}
}

func TestSetEmailClearsVerification(t *testing.T) {
	h := newHarness(t, d1test.Answer{Changes: 1})
	if err := h.store.SetEmail(context.Background(), "user-1", "  Tenant@Example.COM "); err != nil {
		t.Fatalf("SetEmail: %v", err)
	}

	c := h.only()
	// Changing the address must not inherit the old one's proof. Without this
	// clause, pointing an account at an attacker's address would hand over the
	// right to recover it.
	requireSQL(t, c.SQL, "email_verified_at = NULL", "NOT EXISTS")

	if c.Params[1] != "tenant@example.com" {
		t.Errorf("stored address = %#v, want it lowercased and trimmed", c.Params[1])
	}
}

func TestUserByVerifiedEmailIgnoresUnverified(t *testing.T) {
	h := newHarness(t, d1test.Answer{})
	_, err := h.store.UserByVerifiedEmail(context.Background(), "tenant@example.com")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("UserByVerifiedEmail = %v, want ErrNotFound", err)
	}
	// Anyone can type an address they do not own. Only a verified one is
	// evidence, and recovery is the moment that distinction decides who gets
	// an account.
	requireSQL(t, h.only().SQL, "email_verified_at IS NOT NULL")
}

func TestRebindLineAuditsBeforeItMoves(t *testing.T) {
	h := newHarness(t, d1test.Answer{Changes: 1}, d1test.Answer{Changes: 1})
	err := h.store.RebindLine(context.Background(),
		"user-1", "U_old", "U_new", "line_rebind", "", "req-1")
	if err != nil {
		t.Fatalf("RebindLine: %v", err)
	}
	if len(h.fake.Calls) != 2 {
		t.Fatalf("ran %d statements, want 2", len(h.fake.Calls))
	}

	// A stray audit row describing a rebind that did not happen is a question
	// someone can answer later. A rebind with no record of who held the
	// account before is not.
	requireSQL(t, h.fake.Calls[0].SQL, "INSERT INTO identity_audit_logs")
	requireSQL(t, h.fake.Calls[1].SQL,
		"UPDATE identities SET subject = ?2",
		"WHERE provider = 'line' AND user_id = ?1",
	)

	// id, user_id, action, old_subject, new_subject, approved_by, request_id
	if h.fake.Calls[0].Params[3] != "U_old" || h.fake.Calls[0].Params[4] != "U_new" {
		t.Errorf("audit row does not carry both subjects: %#v", h.fake.Calls[0].Params)
	}
	// approved_by is NULL for a self-service rebind, and a name only when a
	// human in the backoffice approved one. Without that distinction an
	// audit log cannot answer the only question it exists for.
	if h.fake.Calls[0].Params[5] != nil {
		t.Errorf("approved_by = %#v, want nil for a self-service rebind", h.fake.Calls[0].Params[5])
	}
	if h.fake.Calls[0].Params[6] != "req-1" {
		t.Errorf("request_id = %#v, want the id that ties this to the access log", h.fake.Calls[0].Params[6])
	}
}

func TestRebindLineFailsWhenNothingMoved(t *testing.T) {
	// The audit row is written and the update matches no identity. Reporting
	// success here would tell a tenant they had recovered an account they
	// still cannot open.
	h := newHarness(t, d1test.Answer{Changes: 1}, d1test.Answer{Changes: 0})
	err := h.store.RebindLine(context.Background(),
		"user-1", "U_old", "U_new", "line_rebind", "", "req-1")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("RebindLine = %v, want ErrNotFound", err)
	}
}

func TestRebindLineStopsWhenTheAuditFails(t *testing.T) {
	h := newHarness(t, d1test.Answer{Failure: "no such table: identity_audit_logs"})
	err := h.store.RebindLine(context.Background(),
		"user-1", "U_old", "U_new", "line_rebind", "", "req-1")
	if err == nil {
		t.Fatal("RebindLine succeeded with a failed audit write")
	}
	if len(h.fake.Calls) != 1 {
		t.Errorf("ran %d statements, want the rebind to stop at the audit", len(h.fake.Calls))
	}
}

func TestIssueAuthTokenStoresNoPlaintext(t *testing.T) {
	h := newHarness(t, d1test.Answer{Changes: 1})
	err := h.store.IssueAuthToken(context.Background(),
		"recovery", "user-1", "the-hash", "tenant@example.com", nowPlus(), "")
	if err != nil {
		t.Fatalf("IssueAuthToken: %v", err)
	}

	c := h.only()
	requireSQL(t, c.SQL, "INSERT INTO auth_tokens", "token_hash")
	for _, p := range c.Params {
		if s, ok := p.(string); ok && strings.Contains(s, "the-raw-token") {
			t.Fatal("a raw token reached the database")
		}
	}
	if c.Params[3] != "the-hash" {
		t.Errorf("token_hash = %#v, want the hash the caller computed", c.Params[3])
	}
	// issued_by is NULL unless a person in the backoffice issued the token, so
	// an audited manual recovery cannot be confused with a self-service one.
	if c.Params[6] != nil {
		t.Errorf("issued_by = %#v, want nil", c.Params[6])
	}
}

func nowPlus() time.Time { return time.Now().Add(15 * time.Minute) }
