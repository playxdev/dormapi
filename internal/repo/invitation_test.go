package repo

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/playxdev/dormapi/internal/d1/d1test"
)

// The hash the backoffice writes for the code A7K9Q2MX under the development
// pepper. Taken from the TypeScript, not from this Go code: if the two ever
// disagree, the QR resolves to nothing and the failure has no error message.
const wantSecretHash = "3JmXWWveJNFfbTC/xZyigKvo1BHnJOwmSKn+QmwpNsk="

// inviteRow is what resolveInvite's query answers with.
func inviteRow(linkedAccount string, confirmed bool) map[string]any {
	row := map[string]any{
		"tenant_id":     testTenant,
		"invitation_id": "01INVITATION00000000000000",
		"contract_id":   testContract,
		"party_id":      testParty,
		"room_id":       "01ROOM0000000000000000000A",
		"rent":          float64(450000),
		"deposit":       float64(900000),
		"start_date":    "2026-10-01",
		"resident_name": "ผู้เช่า ทดสอบ",
		"room_number":   "609",
		"building_name": "Oscar Apartment",
	}
	if linkedAccount != "" {
		row["linked_account"] = linkedAccount
	}
	if confirmed {
		row["confirmed_at"] = "2026-09-01T03:00:00Z"
	}
	return row
}

// The code is a credential. It is never stored in the clear and never sent to
// the database in the clear either: the lookup is by HMAC, so a leaked query
// log is not a set of live keys to other people's rooms.
func TestACodeIsLookedUpByItsHashNeverItsText(t *testing.T) {
	h := newHarness(t, d1test.Answer{Rows: []map[string]any{inviteRow("", false)}})

	if _, err := h.repo.InviteByCode(context.Background(), testAccount,
		"A7K9Q2MX", "1.0", "1.0"); err != nil {
		t.Fatalf("InviteByCode: %v", err)
	}

	c := h.only()
	requireSQL(t, c.SQL, "WHERE i.secret_hash = ?1")
	if c.Params[0] != wantSecretHash {
		t.Errorf("secret_hash = %#v, want the hash the backoffice writes", c.Params[0])
	}
	if hasParam(c.Params, "A7K9Q2MX") {
		t.Error("the plaintext code was sent to the database")
	}
}

// Crockford drops I, L, O and U precisely because people read them as 1, 1, 0
// and V. A code copied off a handover sheet with a dash in it, or typed in
// lower case, has to open the same room.
func TestAMisreadCodeStillResolves(t *testing.T) {
	for _, code := range []string{"A7K9Q2MX", "a7k9q2mx", "A7K9-Q2MX", " a7k9 q2mx "} {
		t.Run(code, func(t *testing.T) {
			h := newHarness(t, d1test.Answer{Rows: []map[string]any{inviteRow("", false)}})
			if _, err := h.repo.InviteByCode(context.Background(), testAccount, code, "1.0", "1.0"); err != nil {
				t.Fatalf("InviteByCode: %v", err)
			}
			if got := h.only().Params[0]; got != wantSecretHash {
				t.Errorf("hash = %#v, want %#v", got, wantSecretHash)
			}
		})
	}
}

// Revoked, expired, and a lease that has ended are all "no such code". A
// resident can do nothing about any of them, and telling them apart would tell
// whoever is probing codes which ones exist.
func TestARevokedOrExpiredCodeOpensNothing(t *testing.T) {
	h := newHarness(t, d1test.Answer{})
	_, err := h.repo.InviteByCode(context.Background(), testAccount, "A7K9Q2MX", "1.0", "1.0")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	requireSQL(t, h.only().SQL,
		"i.status <> 'REVOKED'",
		"i.expires_at > ?2",
		"c.status IN ('ACTIVE', 'ENDING')",
		"t.status = 'ACTIVE'",
	)
}

// The person who already linked this room is told so, rather than being shown
// a review screen for a lease they have already confirmed.
func TestAClaimedInviteIsReportedAsClaimed(t *testing.T) {
	h := newHarness(t, d1test.Answer{Rows: []map[string]any{inviteRow(testAccount, true)}})
	preview, err := h.repo.InviteByCode(context.Background(), testAccount, "A7K9Q2MX", "1.0", "1.0")
	if err != nil {
		t.Fatalf("InviteByCode: %v", err)
	}
	if !preview.AlreadyClaimed || !preview.ClaimedBySelf {
		t.Errorf("preview = %+v, want claimed by self", preview)
	}

	h2 := newHarness(t, d1test.Answer{Rows: []map[string]any{inviteRow("01SOMEONEELSE000000000000", true)}})
	other, err := h2.repo.InviteByCode(context.Background(), testAccount, "A7K9Q2MX", "1.0", "1.0")
	if err != nil {
		t.Fatalf("InviteByCode: %v", err)
	}
	if !other.AlreadyClaimed || other.ClaimedBySelf {
		t.Errorf("preview = %+v, want claimed by somebody else", other)
	}
}

// claimAnswers scripts a full first-time claim: no membership yet, every write
// succeeding.
func claimAnswers() []d1test.Answer {
	return []d1test.Answer{
		{Rows: []map[string]any{inviteRow("", false)}}, // 0 resolve the code
		{},           // 1 look for an existing membership
		{Changes: 1}, // 2 create it
		{Changes: 1}, // 3 membership_event
		{Changes: 1}, // 4 audit membership.created
		{Changes: 1}, // 5 THE GUARD: link the party
		{Changes: 1}, // 6 snapshot the lease
		{Changes: 1}, // 7 consume the invitation
		{Changes: 1}, // 8 redemption receipt
		{Changes: 1}, // 9 consent record
		{Changes: 1}, // 10 audit invitation.redeemed
	}
}

// The order is the design. D1 gives this service no parameterised
// multi-statement write, so single use rests on exactly one statement that can
// succeed only once — the party link — and everything before and after it is
// safe to repeat.
func TestClaimWritesTheGuardBeforeAnythingItAuthorises(t *testing.T) {
	h := newHarness(t, claimAnswers()...)
	if err := h.repo.ClaimInvite(context.Background(), testAccount, "A7K9Q2MX",
		"1.0", "1.0", "req-1"); err != nil {
		t.Fatalf("ClaimInvite: %v", err)
	}

	want := []string{
		"FROM invitation i",
		"SELECT membership_id FROM membership",
		"INSERT INTO membership",
		"INSERT INTO membership_event",
		"INSERT INTO audit_event",
		"UPDATE party",
		"UPDATE contract",
		"UPDATE invitation",
		"INSERT OR IGNORE INTO invitation_redemption",
		"INSERT INTO consent_record",
		"INSERT INTO audit_event",
	}
	if len(h.fake.Calls) != len(want) {
		t.Fatalf("ran %d statements, want %d", len(h.fake.Calls), len(want))
	}
	for i, fragment := range want {
		requireSQL(t, h.fake.Calls[i].SQL, fragment)
	}

	// The guard itself: a party belongs to one account, and the second person
	// to scan the same sheet changes no rows.
	requireSQL(t, h.fake.Calls[5].SQL,
		"UPDATE party",
		"SET account_id = ?3",
		"AND (account_id IS NULL OR account_id = ?3)",
	)
}

// The snapshot copies the lease's own columns rather than numbers sent with
// the request. A confirmation cannot be replayed with a rent nobody was shown.
func TestClaimSnapshotsTheTermsFromTheLeaseItself(t *testing.T) {
	h := newHarness(t, claimAnswers()...)
	if err := h.repo.ClaimInvite(context.Background(), testAccount, "A7K9Q2MX",
		"1.0", "1.0", "req-1"); err != nil {
		t.Fatalf("ClaimInvite: %v", err)
	}

	contract := h.fake.Calls[6]
	requireSQL(t, contract.SQL,
		"agreed_rent = rent",
		"agreed_deposit = deposit",
		"agreed_start_date = start_date",
		"agreed_terms_version = ?4",
		"agreed_pdpa_version = ?5",
		"AND confirmed_at IS NULL",
		"AND status IN ('ACTIVE', 'ENDING')",
	)
	if hasParam(contract.Params, float64(450000)) || hasParam(contract.Params, int64(450000)) {
		t.Error("the rent travelled as a parameter instead of being copied from the row")
	}
}

// The append-only proof of what was accepted (VERTICAL §8). Its hash covers
// the terms that were shown, so the row stays answerable once the template has
// moved on.
func TestClaimRecordsConsent(t *testing.T) {
	h := newHarness(t, claimAnswers()...)
	if err := h.repo.ClaimInvite(context.Background(), testAccount, "A7K9Q2MX",
		"1.0", "1.0", "req-1"); err != nil {
		t.Fatalf("ClaimInvite: %v", err)
	}

	consent := h.fake.Calls[9]
	requireSQL(t, consent.SQL, "INSERT INTO consent_record", "'TENANT_TERMS'", "'ACCEPT'")

	digest := docHash(t, consent.Params)

	// The same terms hash the same way, so the row can be checked against the
	// snapshot years later.
	h2 := newHarness(t, claimAnswers()...)
	if err := h2.repo.ClaimInvite(context.Background(), testAccount, "A7K9Q2MX",
		"1.0", "1.0", "req-2"); err != nil {
		t.Fatalf("ClaimInvite: %v", err)
	}
	if got := docHash(t, h2.fake.Calls[9].Params); got != digest {
		t.Errorf("the same terms hashed to %v and %v", digest, got)
	}
}

// docHash picks the one parameter that is a hex SHA-256.
func docHash(t *testing.T, params []any) string {
	t.Helper()
	for _, p := range params {
		if s, ok := p.(string); ok && len(s) == 64 && strings.Trim(s, "0123456789abcdef") == "" {
			return s
		}
	}
	t.Fatalf("no document hash among the parameters: %#v", params)
	return ""
}

// Somebody else already holds the lease. Refused before a single write, so a
// stolen handover sheet cannot even create a membership.
func TestASecondPersonCannotClaimTheSameLease(t *testing.T) {
	h := newHarness(t, d1test.Answer{
		Rows: []map[string]any{inviteRow("01SOMEONEELSE000000000000", true)},
	})
	err := h.repo.ClaimInvite(context.Background(), testAccount, "A7K9Q2MX", "1.0", "1.0", "req-1")
	if !errors.Is(err, ErrAlreadyClaimed) {
		t.Fatalf("err = %v, want ErrAlreadyClaimed", err)
	}
	if len(h.fake.Calls) != 1 {
		t.Errorf("wrote something for a lease that was not theirs: %#v", h.fake.Calls[1:])
	}
}

// The guard itself matching nothing — the row was taken between the read and
// the write. The race is exactly what the guard exists for.
func TestClaimStopsWhenTheGuardMatchesNothing(t *testing.T) {
	answers := claimAnswers()
	answers[5] = d1test.Answer{Changes: 0}
	h := newHarness(t, answers...)

	err := h.repo.ClaimInvite(context.Background(), testAccount, "A7K9Q2MX", "1.0", "1.0", "req-1")
	if !errors.Is(err, ErrAlreadyClaimed) {
		t.Fatalf("err = %v, want ErrAlreadyClaimed", err)
	}
	if len(h.fake.Calls) != 6 {
		t.Fatalf("ran %d statements, want to stop at the guard", len(h.fake.Calls))
	}
}

// A resident whose first attempt failed halfway retries. Their own account
// still matches the guard, so the same call finishes the work instead of being
// refused as a duplicate — the alternative is a person with a membership, no
// lease, and no way to try again.
func TestAResumedClaimFinishesRatherThanRefusing(t *testing.T) {
	h := newHarness(t,
		d1test.Answer{Rows: []map[string]any{inviteRow(testAccount, false)}},
		d1test.Answer{Rows: []map[string]any{{"membership_id": "01MEMBER00000000000000000A"}}},
		d1test.Answer{Changes: 1}, // the guard still matches: account_id = caller
		d1test.Answer{Changes: 1},
		d1test.Answer{Changes: 1},
		d1test.Answer{Changes: 1},
		d1test.Answer{Changes: 1},
		d1test.Answer{Changes: 1},
	)
	if err := h.repo.ClaimInvite(context.Background(), testAccount, "A7K9Q2MX",
		"1.0", "1.0", "req-1"); err != nil {
		t.Fatalf("a retry was refused: %v", err)
	}
	requireSQL(t, h.fake.Calls[2].SQL, "UPDATE party")
}

// A second lease at the same operator hangs off a second party, not a second
// membership: `ux_membership_occupancy` allows one live CLIENT membership per
// account per tenant.
func TestASecondLeaseAtTheSameOperatorReusesTheMembership(t *testing.T) {
	h := newHarness(t,
		d1test.Answer{Rows: []map[string]any{inviteRow("", false)}},
		d1test.Answer{Rows: []map[string]any{{"membership_id": "01MEMBER00000000000000000A"}}},
		d1test.Answer{Changes: 1},
		d1test.Answer{Changes: 1},
		d1test.Answer{Changes: 1},
		d1test.Answer{Changes: 1},
		d1test.Answer{Changes: 1},
		d1test.Answer{Changes: 1},
	)
	if err := h.repo.ClaimInvite(context.Background(), testAccount, "A7K9Q2MX",
		"1.0", "1.0", "req-1"); err != nil {
		t.Fatalf("ClaimInvite: %v", err)
	}
	for _, c := range h.fake.Calls {
		if strings.Contains(c.SQL, "INSERT INTO membership\n") ||
			strings.Contains(c.SQL, "INSERT INTO membership ") {
			t.Error("created a second membership for the same operator")
		}
	}
}

// The claim writes to one tenant: the one the code resolved to. There is no
// path by which a tenant_id from the request could reach any of these
// statements.
func TestEveryClaimWriteBindsTheTenantTheCodeResolvedTo(t *testing.T) {
	h := newHarness(t, claimAnswers()...)
	if err := h.repo.ClaimInvite(context.Background(), testAccount, "A7K9Q2MX",
		"1.0", "1.0", "req-1"); err != nil {
		t.Fatalf("ClaimInvite: %v", err)
	}
	for i, c := range h.fake.Calls[1:] {
		if !hasParam(c.Params, testTenant) {
			t.Errorf("statement %d carries no tenant: %s\n%#v", i+1, c.SQL, c.Params)
		}
	}
}
