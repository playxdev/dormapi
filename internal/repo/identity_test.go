package repo

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/playxdev/dormapi/internal/d1/d1test"
)

// The identity carries the provider it was issued under, because that is what
// a LINE userId is unique within. The Login channel and the Messaging API
// channel see one person as one userId only because both sit under one
// provider; scoping by channel would file them twice.
func TestALineIdentityIsScopedToItsProvider(t *testing.T) {
	h := newHarness(t, d1test.Answer{Rows: []map[string]any{{
		"account_id": testAccount, "display_name": "ผู้เช่า ทดสอบ",
	}}})

	account, err := h.repo.AccountByLine(context.Background(), "U_line_subject", "ผู้เช่า ทดสอบ")
	if err != nil {
		t.Fatalf("AccountByLine: %v", err)
	}
	if account.ID != testAccount {
		t.Errorf("account = %+v", account)
	}

	c := h.only()
	requireSQL(t, c.SQL, "i.provider = 'LINE'", "i.provider_scope = ?1", "i.external_id = ?2")
	if c.Params[0] != "2011358311" {
		t.Errorf("provider_scope = %#v, want the provider", c.Params[0])
	}
}

// First sign-in. The account row goes in before the identity, because the
// identity has a foreign key to it, and the identity is then what decides who
// the account is.
func TestFirstSignInCreatesAnAccountAndItsIdentity(t *testing.T) {
	h := newHarness(t,
		d1test.Answer{},           // no identity yet
		d1test.Answer{Changes: 1}, // create the account
		d1test.Answer{Changes: 1}, // link the identity
		// Read back through the identity, which now points at the account just
		// created. Its id was minted inside the call, so the answer has to be
		// built from what was actually sent.
		d1test.Answer{From: func(prior []d1test.Call) []map[string]any {
			return []map[string]any{{
				"account_id":   prior[1].Params[0],
				"display_name": "ผู้เช่า ทดสอบ",
			}}
		}},
	)

	account, err := h.repo.AccountByLine(context.Background(), "U_new", "ผู้เช่า ทดสอบ")
	if err != nil {
		t.Fatalf("AccountByLine: %v", err)
	}

	requireSQL(t, h.fake.Calls[1].SQL, "INSERT INTO account", "'ACTIVE'")
	requireSQL(t, h.fake.Calls[2].SQL, "INSERT OR IGNORE INTO account_identity", "'LINE'")

	// The account row carries no tenant, no role and no LINE id (rule 1).
	for _, forbidden := range []string{"tenant_id", "role", "line_user_id", "phone"} {
		if strings.Contains(h.fake.Calls[1].SQL, forbidden) {
			t.Errorf("the account insert names %q", forbidden)
		}
	}

	if account.ID != h.fake.Calls[1].Params[0] {
		t.Errorf("account = %+v, want the one the identity resolves to", account)
	}
	// Nothing to clean up: the account created here is the one that won.
	if len(h.fake.Calls) != 4 {
		t.Errorf("ran %d statements, want no cleanup on the ordinary path: %#v",
			len(h.fake.Calls), h.fake.Calls)
	}
}

// Two first sign-ins race, or a retry lands after a partial failure. The
// identity's unique index decides, and the account row that lost is taken back
// while nothing references it — one person must not become two accounts.
func TestARacedFirstSignInDoesNotLeaveTwoAccounts(t *testing.T) {
	h := newHarness(t,
		d1test.Answer{},           // no identity yet
		d1test.Answer{Changes: 1}, // create an account
		d1test.Answer{Changes: 0}, // OR IGNORE: somebody else already holds the identity
		d1test.Answer{Rows: []map[string]any{{
			"account_id": "01WINNER0000000000000000AA", "display_name": "ผู้เช่า ทดสอบ",
		}}},
		d1test.Answer{Changes: 1}, // discard the account that lost
	)

	account, err := h.repo.AccountByLine(context.Background(), "U_new", "ผู้เช่า ทดสอบ")
	if err != nil {
		t.Fatalf("AccountByLine: %v", err)
	}
	if account.ID != "01WINNER0000000000000000AA" {
		t.Errorf("account = %+v, want the one holding the identity", account)
	}

	cleanup := h.fake.Calls[4]
	requireSQL(t, cleanup.SQL,
		"DELETE FROM account",
		"NOT EXISTS (SELECT 1 FROM account_identity WHERE account_id = ?1)",
		"NOT EXISTS (SELECT 1 FROM membership WHERE account_id = ?1)",
	)
}

// The tenant is chosen from the caller's own memberships and from nothing
// else. A suspended membership still resolves — it must be able to see why.
func TestResolveTenancyReadsTheTenantFromMembership(t *testing.T) {
	h := newHarness(t, d1test.Answer{Rows: []map[string]any{{
		"tenant_id": testTenant, "membership_id": "01MEMBER00000000000000000A",
		"operator_name": "หอพักทดสอบ", "party_id": testParty,
		"account_id": testAccount, "display_name": "ผู้เช่า ทดสอบ",
		"email": "tenant@example.com", "verified_at": "2026-09-01T00:00:00Z",
		"contract_id": testContract, "building_id": "01BUILDING", "building_name": "Oscar",
		"room_id": "01ROOM", "room_number": "609",
	}}})

	tenancy, err := h.repo.ResolveTenancy(context.Background(), testAccount)
	if err != nil {
		t.Fatalf("ResolveTenancy: %v", err)
	}

	c := h.only()
	requireSQL(t, c.SQL,
		"FROM membership m",
		"m.account_id = ?1",
		"m.kind = 'CLIENT'",
		"m.status IN ('ACTIVE', 'SUSPENDED')",
		"t.status = 'ACTIVE'",
		"c.status IN ('ACTIVE', 'ENDING')",
		// A person may rent from two operators. Until the app offers a
		// switcher the answer must at least be the same one every time.
		"ORDER BY c.start_date DESC, c.contract_id DESC",
		"LIMIT 1",
	)
	if len(c.Params) != 1 || c.Params[0] != testAccount {
		t.Errorf("params = %#v, want the account alone", c.Params)
	}

	if tenancy.tenantID != testTenant || tenancy.partyID != testParty {
		t.Errorf("tenancy = %+v", tenancy)
	}
	if tenancy.ResidentID != testParty || tenancy.OperatorName != "หอพักทดสอบ" {
		t.Errorf("tenancy = %+v", tenancy)
	}
	if !tenancy.Account.Verified {
		t.Error("a verified address was read as unverified")
	}
}

// Signed in, no room. The app shows "your account is not linked to a
// dormitory" rather than an error.
func TestResolveTenancyIsNotFoundWithoutALease(t *testing.T) {
	h := newHarness(t, d1test.Answer{})
	_, err := h.repo.ResolveTenancy(context.Background(), testAccount)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// An address is proof of nothing until a link sent to it comes back. Changing
// it must not inherit the old one's proof, or changing it to an attacker's
// address would inherit the right to recover the account.
func TestANewAddressStartsUnverified(t *testing.T) {
	h := newHarness(t,
		d1test.Answer{},           // nobody holds it
		d1test.Answer{Changes: 1}, // insert
		d1test.Answer{Changes: 1}, // retire the old one
	)
	if err := h.repo.SetEmail(context.Background(), testAccount, "tenant@example.com"); err != nil {
		t.Fatalf("SetEmail: %v", err)
	}
	requireSQL(t, h.fake.Calls[1].SQL, "INSERT INTO account_identity", "'EMAIL'", "NULL, 0")
	requireSQL(t, h.fake.Calls[2].SQL, "SET deleted_at = ?3", "external_id <> ?2")
}

func TestAnAddressAlreadyHeldElsewhereIsRefused(t *testing.T) {
	h := newHarness(t, d1test.Answer{Rows: []map[string]any{{
		"identity_id": "01IDENTITY", "account_id": "01SOMEONEELSE000000000000",
	}}})
	err := h.repo.SetEmail(context.Background(), testAccount, "tenant@example.com")
	if !errors.Is(err, ErrEmailTaken) {
		t.Fatalf("err = %v, want ErrEmailTaken", err)
	}
	if len(h.fake.Calls) != 1 {
		t.Errorf("wrote something for somebody else's address: %#v", h.fake.Calls[1:])
	}
}

// Re-submitting the address already on the account is how a resident asks for
// another verification mail. It must not be refused as taken by "somebody" who
// is themselves.
func TestResubmittingYourOwnAddressAsksForAnotherMail(t *testing.T) {
	h := newHarness(t,
		d1test.Answer{Rows: []map[string]any{{
			"identity_id": "01IDENTITY", "account_id": testAccount,
		}}},
		d1test.Answer{Changes: 1},
	)
	if err := h.repo.SetEmail(context.Background(), testAccount, "tenant@example.com"); err != nil {
		t.Fatalf("SetEmail: %v", err)
	}
	requireSQL(t, h.fake.Calls[1].SQL, "SET verified_at = NULL")
}

// Only a verified address is evidence. Anyone can type an address they do not
// own, and recovery is exactly the moment that distinction matters.
func TestRecoveryLooksUpVerifiedAddressesOnly(t *testing.T) {
	h := newHarness(t, d1test.Answer{})
	_, err := h.repo.AccountByVerifiedEmail(context.Background(), "  Tenant@Example.COM ")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	c := h.only()
	requireSQL(t, c.SQL, "i.verified_at IS NOT NULL", "a.status = 'ACTIVE'")
	if c.Params[0] != "tenant@example.com" {
		t.Errorf("address = %#v, want it normalised before the lookup", c.Params[0])
	}
}

// A single-use link. The update matches only while consumed_at is NULL, so two
// requests racing the same link cannot both win.
func TestATokenIsSpentExactlyOnce(t *testing.T) {
	h := newHarness(t, d1test.Answer{Rows: []map[string]any{{
		"account_id": testAccount, "sent_to": "tenant@example.com",
	}}})
	accountID, sentTo, err := h.repo.ConsumeAuthToken(context.Background(), "RECOVERY", "a-hash")
	if err != nil {
		t.Fatalf("ConsumeAuthToken: %v", err)
	}
	if accountID != testAccount || sentTo != "tenant@example.com" {
		t.Errorf("got %q / %q", accountID, sentTo)
	}
	requireSQL(t, h.only().SQL,
		"UPDATE auth_token SET consumed_at",
		"AND consumed_at IS NULL",
		"AND expires_at > ?3",
		"RETURNING account_id, sent_to",
	)
}

// A rebind moves one row and touches nothing else: the membership, the party,
// the lease and the payment history stay where they are. That is the whole
// difference between recovery and starting again.
func TestRebindMovesOnlyTheCredential(t *testing.T) {
	h := newHarness(t,
		d1test.Answer{Changes: 1}, // audit first
		d1test.Answer{Changes: 1}, // move the identity
	)
	if err := h.repo.RebindLine(context.Background(), testAccount, "U_old", "U_new", "req-1"); err != nil {
		t.Fatalf("RebindLine: %v", err)
	}
	requireSQL(t, h.fake.Calls[0].SQL, "INSERT INTO audit_event")
	requireSQL(t, h.fake.Calls[1].SQL,
		"UPDATE account_identity",
		"SET external_id = ?3",
		"provider = 'LINE'",
		"provider_scope = ?1",
		"account_id = ?2",
	)
	for _, c := range h.fake.Calls {
		for _, forbidden := range []string{"membership", "party", "contract", "payment"} {
			if strings.Contains(c.SQL, forbidden) {
				t.Errorf("a rebind touched %q", forbidden)
			}
		}
	}
}

// The LINE account presenting the token already signs in as somebody else.
// Moving it would take that person's account away from them.
func TestRebindRefusesALineAccountAlreadyInUse(t *testing.T) {
	h := newHarness(t,
		d1test.Answer{Changes: 1},
		d1test.Answer{Failure: "UNIQUE constraint failed: account_identity.external_id"},
	)
	err := h.repo.RebindLine(context.Background(), testAccount, "U_old", "U_taken", "req-1")
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("err = %v, want ErrConflict", err)
	}
}
