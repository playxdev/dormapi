package repo

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/playxdev/dormapi/internal/ulid"
)

// InvitePreview is what the resident reviews before confirming. It is the
// exact set of terms the confirmation is taken to cover.
//
// LeaseURL points at the backoffice's rendering of this lease — the same
// wording the operator prints. The money terms are repeated here because the
// app leads with them; the clauses are not, because a second copy of a lease
// is a second lease.
type InvitePreview struct {
	Code          string `json:"code"`
	LeaseURL      string `json:"lease_url,omitempty"`
	TermsVersion  string `json:"terms_version"`
	PDPAVersion   string `json:"pdpa_version"`
	BuildingName  string `json:"building_name"`
	RoomNumber    string `json:"room_number"`
	ResidentName  string `json:"resident_name"`
	RentSatang    int64  `json:"rent_satang"`
	DepositSatang int64  `json:"deposit_satang"`
	StartDate     string `json:"start_date"`

	AlreadyClaimed bool `json:"already_claimed"`
	ClaimedBySelf  bool `json:"claimed_by_self"`
}

/*
Unscoped — invitations.

Resolving a code takes no [Tenancy] and cannot: the code is the only
credential its holder has, and which operator it belongs to is the answer, not
the question. `ux_invitation_secret` is globally unique for exactly this
reason (STANDARD §7.4).

Everything downstream uses the tenant_id found here — never one that arrived
with the request.
*/

// invite is a resolved code: which operator, which invitation, which lease,
// and who the operator says the resident is.
type invite struct {
	tenantID     string
	invitationID string
	contractID   string
	partyID      string
	roomID       string

	linkedAccount string
	confirmed     bool

	buildingName string
	roomNumber   string
	residentName string
	rent         int64
	deposit      int64
	startDate    string
}

// resolveInvite finds what a code opens.
//
// Revoked and expired codes match nothing, and neither does a lease that has
// ended: a resident can do nothing about any of them, and distinguishing the
// cases would tell whoever is probing codes which ones exist.
//
// A code whose invitation is EXHAUSTED still resolves. Single use is enforced
// by the party link below, not here, and this way the app can say "you already
// linked this room" instead of "no such code" to the person who just did.
func (r *Repo) resolveInvite(ctx context.Context, code string) (*invite, error) {
	res, err := r.db.Query(ctx, `
		SELECT i.tenant_id, i.invitation_id, ci.contract_id,
		       c.party_id, c.room_id, c.rent, c.deposit, c.start_date, c.confirmed_at,
		       p.display_name AS resident_name, p.account_id AS linked_account,
		       rm.number AS room_number, b.name AS building_name
		FROM invitation i
		JOIN contract_invitation ci
		     ON ci.tenant_id = i.tenant_id AND ci.invitation_id = i.invitation_id
		JOIN contract c
		     ON c.tenant_id = ci.tenant_id AND c.contract_id = ci.contract_id
		     AND c.status IN ('ACTIVE', 'ENDING') AND c.deleted_at IS NULL
		JOIN party p  ON p.tenant_id = c.tenant_id AND p.party_id = c.party_id
		     AND p.deleted_at IS NULL
		JOIN room rm  ON rm.tenant_id = c.tenant_id AND rm.room_id = c.room_id
		JOIN building b ON b.tenant_id = rm.tenant_id AND b.building_id = rm.building_id
		JOIN tenant t ON t.tenant_id = i.tenant_id
		     AND t.status = 'ACTIVE' AND t.deleted_at IS NULL
		WHERE i.secret_hash = ?1
		  AND i.status <> 'REVOKED'
		  AND i.expires_at > ?2`,
		r.pepper.Hash(ulid.NormalizeCrockford(code)), now())
	if err != nil {
		return nil, fmt.Errorf("repo: resolve invite: %w", err)
	}
	if len(res.Results) == 0 {
		return nil, ErrNotFound
	}

	row := res.Results[0]
	return &invite{
		tenantID:      text(row["tenant_id"]),
		invitationID:  text(row["invitation_id"]),
		contractID:    text(row["contract_id"]),
		partyID:       text(row["party_id"]),
		roomID:        text(row["room_id"]),
		linkedAccount: text(row["linked_account"]),
		confirmed:     text(row["confirmed_at"]) != "",
		buildingName:  text(row["building_name"]),
		roomNumber:    text(row["room_number"]),
		residentName:  text(row["resident_name"]),
		rent:          number(row["rent"]),
		deposit:       number(row["deposit"]),
		startDate:     text(row["start_date"]),
	}, nil
}

// InviteByCode reads an invitation for review.
func (r *Repo) InviteByCode(ctx context.Context, accountID, code, termsVersion, pdpaVersion string) (*InvitePreview, error) {
	inv, err := r.resolveInvite(ctx, code)
	if err != nil {
		return nil, err
	}
	return &InvitePreview{
		Code:           strings.ToUpper(code),
		LeaseURL:       r.leaseURL(code),
		TermsVersion:   termsVersion,
		PDPAVersion:    pdpaVersion,
		BuildingName:   inv.buildingName,
		RoomNumber:     inv.roomNumber,
		ResidentName:   inv.residentName,
		RentSatang:     inv.rent,
		DepositSatang:  inv.deposit,
		StartDate:      inv.startDate,
		AlreadyClaimed: inv.linkedAccount != "",
		ClaimedBySelf:  inv.linkedAccount != "" && inv.linkedAccount == accountID,
	}, nil
}

// ClaimInvite binds the caller to the lease the code opens.
//
// # What makes it happen once
//
// The pre-XYZ claim was a single UPDATE guarded on `confirmed_by_user_id IS
// NULL`. That column is gone: under XYZ the link between a person and a
// resident record lives on `party` (VERTICAL §4), and the claim needs six
// writes rather than one — a membership, the party link, the lease snapshot,
// the invitation counter, the redemption receipt and the consent record.
//
// The backoffice would do that as a `batch()` with the guard first. This
// service cannot: D1's REST API refuses parameters when more than one
// statement is sent (see internal/d1), and a batch built by string
// concatenation is an injection waiting to happen. So the writes are ordered
// so that exactly one of them can only succeed once, and every other write is
// safe to repeat:
//
//  1. membership   INSERT ... WHERE NOT EXISTS  — repeatable
//  2. party link   UPDATE ... WHERE account_id IS NULL OR account_id = caller
//     — THE GUARD
//  3. lease        UPDATE ... WHERE confirmed_at IS NULL   — repeatable
//  4. invitation   UPDATE ... WHERE used_count < max_usage — repeatable
//  5. redemption   INSERT OR IGNORE (unique per invitation+account) — repeatable
//  6. consent      INSERT — see below
//  7. audit        INSERT — append-only by design
//
// Step 2 is the whole of single use: a party belongs to one account, and the
// second person to scan the same sheet changes no rows and is told the room is
// taken. A failure anywhere after it leaves the caller able to retry — their
// own account still matches the guard, so the same call finishes the work
// rather than being refused as a duplicate.
//
// Step 1 runs before the guard because `party.membership_id` has a foreign key
// to it. A membership created for a claim that then loses the guard is a
// person who can open the app and sees no lease, which is the state every
// resident is in before they scan anything.
//
// # The snapshot
//
// agreed_* copies the lease's values at this moment rather than referencing
// them. If the operator later amends the rent, the resident's record of what
// they agreed to must not move with it — that is the answer to "what rent did
// I agree to?" eleven months later (VERTICAL §8).
func (r *Repo) ClaimInvite(ctx context.Context, accountID, code, termsVersion, pdpaVersion, requestID string) error {
	inv, err := r.resolveInvite(ctx, code)
	if err != nil {
		return err
	}
	if inv.linkedAccount != "" && inv.linkedAccount != accountID {
		return ErrAlreadyClaimed
	}

	membershipID, err := r.clientMembership(ctx, inv.tenantID, accountID, inv.invitationID, requestID)
	if err != nil {
		return err
	}

	ts := now()

	// The guard. Everything above this line is repeatable; everything below it
	// is only reached by the one account that holds the party.
	res, err := r.db.Query(ctx, `
		UPDATE party
		   SET account_id = ?3, membership_id = ?4, linked_at = ?5,
		       updated_at = ?5, version = version + 1
		 WHERE tenant_id = ?1 AND party_id = ?2
		   AND deleted_at IS NULL
		   AND (account_id IS NULL OR account_id = ?3)`,
		inv.tenantID, inv.partyID, accountID, membershipID, ts)
	if err != nil {
		return fmt.Errorf("repo: link party: %w", err)
	}
	if res.Meta.Changes == 0 {
		return ErrAlreadyClaimed
	}

	if _, err := r.db.Query(ctx, `
		UPDATE contract
		   SET confirmed_at         = ?3,
		       agreed_rent          = rent,
		       agreed_deposit       = deposit,
		       agreed_start_date    = start_date,
		       agreed_terms_version = ?4,
		       agreed_pdpa_version  = ?5,
		       updated_at           = ?3,
		       version              = version + 1
		 WHERE tenant_id = ?1 AND contract_id = ?2
		   AND confirmed_at IS NULL
		   AND status IN ('ACTIVE', 'ENDING')
		   AND deleted_at IS NULL`,
		inv.tenantID, inv.contractID, ts, termsVersion, pdpaVersion); err != nil {
		return fmt.Errorf("repo: confirm contract: %w", err)
	}

	// Bookkeeping, not the guard. An invitation at its limit flips to
	// EXHAUSTED so the backoffice's invitation screen stops offering it.
	if _, err := r.db.Query(ctx, `
		UPDATE invitation
		   SET used_count = used_count + 1,
		       status = CASE WHEN used_count + 1 >= max_usage THEN 'EXHAUSTED' ELSE status END
		 WHERE tenant_id = ?1 AND invitation_id = ?2
		   AND status = 'ACTIVE'
		   AND used_count < max_usage`,
		inv.tenantID, inv.invitationID); err != nil {
		return fmt.Errorf("repo: consume invitation: %w", err)
	}

	// Append-only receipt (rule 9). `ux_redemption_once` is unique per
	// (invitation, account) for a SUCCESS, so a retry writes nothing.
	if _, err := r.db.Query(ctx, `
		INSERT OR IGNORE INTO invitation_redemption
			(tenant_id, invitation_id, redemption_id, account_id, membership_id,
			 result, redeemed_at)
		VALUES (?1, ?2, ?3, ?4, ?5, 'SUCCESS', ?6)`,
		inv.tenantID, inv.invitationID, ulid.New(), accountID, membershipID, ts); err != nil {
		return fmt.Errorf("repo: record redemption: %w", err)
	}

	if err := r.recordConsent(ctx, inv, accountID, termsVersion, pdpaVersion, ts); err != nil {
		return err
	}

	return r.audit(ctx, auditEvent{
		TenantID:       inv.tenantID,
		ActorType:      "CLIENT",
		ActorAccountID: accountID,
		Action:         "invitation.redeemed",
		TargetType:     "contract",
		TargetID:       inv.contractID,
		RequestID:      requestID,
	})
}

// clientMembership returns the caller's CLIENT membership at this operator,
// creating it if this is their first lease there.
//
// `ux_membership_occupancy` allows one live CLIENT membership per account per
// tenant, so a resident taking a second room at the same operator reuses this
// one — the second lease hangs off a second party, not a second membership.
func (r *Repo) clientMembership(ctx context.Context, tenantID, accountID, invitationID, requestID string) (string, error) {
	existing, err := r.findClientMembership(ctx, tenantID, accountID)
	if err == nil {
		return existing, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return "", err
	}

	membershipID := ulid.New()
	ts := now()
	res, err := r.db.Query(ctx, `
		INSERT INTO membership
			(tenant_id, membership_id, account_id, kind, status, invitation_id,
			 joined_at, created_at, updated_at)
		SELECT ?1, ?2, ?3, 'CLIENT', 'ACTIVE', ?4, ?5, ?5, ?5
		WHERE NOT EXISTS (
			SELECT 1 FROM membership
			 WHERE tenant_id = ?1 AND account_id = ?3 AND kind = 'CLIENT'
			   AND status IN ('INVITED','ACTIVE','SUSPENDED','RELEASE_PENDING')
			   AND deleted_at IS NULL)`,
		tenantID, membershipID, accountID, invitationID, ts)
	if err != nil && !isUnique(err) {
		return "", fmt.Errorf("repo: create membership: %w", err)
	}
	if err != nil || res.Meta.Changes == 0 {
		// The NOT EXISTS matched, or the occupancy index refused a racing
		// insert. Either way somebody else's row is the one the party must
		// reference, so read it rather than trust the id just minted.
		return r.findClientMembership(ctx, tenantID, accountID)
	}

	// Every status change carries actor, time and reason (rule 8).
	if _, err := r.db.Query(ctx, `
		INSERT INTO membership_event
			(tenant_id, membership_id, event_id, from_status, to_status,
			 actor_type, actor_account_id, reason, occurred_at)
		VALUES (?1, ?2, ?3, NULL, 'ACTIVE', 'CLIENT', ?4, 'invitation.redeemed', ?5)`,
		tenantID, membershipID, ulid.New(), accountID, now()); err != nil {
		return "", fmt.Errorf("repo: record membership event: %w", err)
	}
	if err := r.audit(ctx, auditEvent{
		TenantID:       tenantID,
		ActorType:      "CLIENT",
		ActorAccountID: accountID,
		Action:         "membership.created",
		TargetType:     "membership",
		TargetID:       membershipID,
		RequestID:      requestID,
	}); err != nil {
		return "", err
	}
	return membershipID, nil
}

func (r *Repo) findClientMembership(ctx context.Context, tenantID, accountID string) (string, error) {
	res, err := r.db.Query(ctx, `
		SELECT membership_id FROM membership
		WHERE tenant_id = ?1 AND account_id = ?2 AND kind = 'CLIENT'
		  AND status IN ('INVITED','ACTIVE','SUSPENDED','RELEASE_PENDING')
		  AND deleted_at IS NULL
		LIMIT 1`, tenantID, accountID)
	if err != nil {
		return "", fmt.Errorf("repo: find membership: %w", err)
	}
	if len(res.Results) == 0 {
		return "", ErrNotFound
	}
	return text(res.Results[0]["membership_id"]), nil
}

// recordConsent writes the append-only proof of what was accepted.
//
// VERTICAL §8: the agreed_* snapshot "is also the CONSENT_RECORD (doc_type =
// TENANT_TERMS, with doc_hash)". One row, not two — the PDPA notice is clause
// 8 of the same document, and its version is recorded on the lease as
// agreed_pdpa_version.
//
// doc_hash is meant to be "SHA-256 of the document the user actually saw". The
// document is rendered by the backoffice from the lease's own numbers, so this
// service has no stable bytes to hash: the rendered HTML changes with a
// stylesheet edit that changes no term. What it hashes instead is the exact
// set of terms it sent to the app — the versions, the money, the date, the
// room — in a fixed order. That is reproducible from the stored snapshot years
// later, which is the question a doc_hash exists to answer, and it is honest
// about covering the terms rather than the typography.
func (r *Repo) recordConsent(ctx context.Context, inv *invite, accountID, termsVersion, pdpaVersion, ts string) error {
	digest := sha256.Sum256([]byte(strings.Join([]string{
		"dorm.place/lease",
		"terms=" + termsVersion,
		"pdpa=" + pdpaVersion,
		"contract=" + inv.contractID,
		"room=" + inv.roomID,
		"rent=" + fmt.Sprint(inv.rent),
		"deposit=" + fmt.Sprint(inv.deposit),
		"start=" + inv.startDate,
	}, "\n")))

	if _, err := r.db.Query(ctx, `
		INSERT INTO consent_record
			(consent_id, account_id, tenant_id, doc_type, doc_version, doc_hash,
			 action, locale, occurred_at)
		VALUES (?1, ?2, ?3, 'TENANT_TERMS', ?4, ?5, 'ACCEPT', 'th', ?6)`,
		ulid.New(), accountID, inv.tenantID, termsVersion,
		hex.EncodeToString(digest[:]), ts); err != nil {
		return fmt.Errorf("repo: record consent: %w", err)
	}
	return nil
}

// leaseURL addresses the backoffice's rendering of this invitation's lease.
//
// The code is the whole authorisation: that page shows no more than the person
// holding the code can already do with it, and masks the id number besides.
func (r *Repo) leaseURL(code string) string {
	if r.backofficeURL == "" || code == "" {
		return ""
	}
	return r.backofficeURL + "/lease/" + url.PathEscape(code)
}
