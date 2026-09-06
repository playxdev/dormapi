package store

import (
	"context"
	"crypto/rand"
	"fmt"
	"strings"
)

// inviteAlphabet omits characters that are read wrong when a code is spoken
// over the phone or copied off a screen: 0/O, 1/I/L, 2/Z, 5/S, 8/B.
//
// This is not hypothetical. A LIFF ID transcribed from a screenshot in this
// project cost several rounds of debugging because a lowercase l was read as an
// uppercase I.
const inviteAlphabet = "34679ACDEFGHJKMNPQRTUVWXY"

const inviteCodeLength = 8

// NewInviteCode returns an unguessable, unambiguous invite code.
func NewInviteCode() (string, error) {
	buf := make([]byte, inviteCodeLength)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("store: generate invite code: %w", err)
	}

	var code strings.Builder
	for _, b := range buf {
		// The alphabet length does not divide 256 evenly, so this is very
		// slightly biased. With 25^8 codes and single use plus expiry, that bias
		// is irrelevant to guessability here.
		code.WriteByte(inviteAlphabet[int(b)%len(inviteAlphabet)])
	}
	return code.String(), nil
}

// InvitePreview is what the tenant reviews before confirming. It is the exact
// set of terms the confirmation is taken to cover.
type InvitePreview struct {
	Code           string `json:"code"`
	BuildingName   string `json:"building_name"`
	RoomNumber     string `json:"room_number"`
	TenantName     string `json:"tenant_name"`
	RentSatang     int64  `json:"rent_satang"`
	DepositSatang  int64  `json:"deposit_satang"`
	StartDate      string `json:"start_date"`
	AlreadyClaimed bool   `json:"already_claimed"`
	ClaimedBySelf  bool   `json:"claimed_by_self"`
}

// InviteByCode reads an invite for review.
//
// Expired, revoked and non-active contracts are reported as not found: a
// tenant can do nothing about any of them, and distinguishing the cases would
// tell whoever is probing codes which ones exist.
func (s *Store) InviteByCode(ctx context.Context, userID, code string) (*InvitePreview, error) {
	res, err := s.db.Query(ctx, `
		SELECT i.code, b.name AS building_name, r.number AS room_number,
		       tn.name AS tenant_name, c.rent, c.deposit, c.start_date,
		       c.confirmed_by_user_id
		FROM invites i
		JOIN contracts c ON c.id = i.contract_id
		JOIN tenants tn  ON tn.id = c.tenant_id
		JOIN rooms r     ON r.id = c.room_id
		JOIN buildings b ON b.id = r.building_id
		WHERE i.code = ?1
		  AND i.revoked_at IS NULL
		  AND i.expires_at > datetime('now')
		  AND c.status = 'active'`, code)
	if err != nil {
		return nil, fmt.Errorf("store: get invite: %w", err)
	}
	if len(res.Results) == 0 {
		return nil, ErrNotFound
	}

	row := res.Results[0]
	claimedBy := text(row["confirmed_by_user_id"])
	return &InvitePreview{
		Code:           text(row["code"]),
		BuildingName:   text(row["building_name"]),
		RoomNumber:     text(row["room_number"]),
		TenantName:     text(row["tenant_name"]),
		RentSatang:     number(row["rent"]),
		DepositSatang:  number(row["deposit"]),
		StartDate:      text(row["start_date"]),
		AlreadyClaimed: claimedBy != "",
		ClaimedBySelf:  claimedBy != "" && claimedBy == userID,
	}, nil
}

// ClaimInvite attaches the caller's account to the contract's tenant record and
// stores the terms they confirmed.
//
// One statement — the only shape D1 allows for a parameterised write, and now
// the only shape this needs. Single use falls out of the guard: the update
// matches only while confirmed_by_user_id is still NULL, so a second claim
// changes no rows.
//
// The agreed_* columns copy the contract's values at this moment rather than
// referencing them. If the owner later amends the rent, the tenant's record of
// what they agreed to must not move with it. agreed_terms_version and
// agreed_pdpa_version name the documents the tenant was shown, so a
// confirmation stays answerable after the template changes.
func (s *Store) ClaimInvite(ctx context.Context, userID, code string, termsVersion, pdpaVersion string) error {
	res, err := s.db.Query(ctx, `
		UPDATE contracts SET
			confirmed_by_user_id = ?1,
			confirmed_at         = datetime('now'),
			agreed_rent          = rent,
			agreed_deposit       = deposit,
			agreed_start_date    = start_date,
			agreed_terms_version = ?3,
			agreed_pdpa_version  = ?4
		WHERE confirmed_by_user_id IS NULL
		  AND status = 'active'
		  AND id = (
			SELECT i.contract_id FROM invites i
			WHERE i.code = ?2
			  AND i.revoked_at IS NULL
			  AND i.expires_at > datetime('now')
		  )`, userID, code, termsVersion, pdpaVersion)
	if err != nil {
		return fmt.Errorf("store: claim invite: %w", err)
	}
	if res.Meta.Changes == 0 {
		// Either the invite is unusable, or the contract is already confirmed.
		// Distinguish so the app can say "you already linked this room".
		preview, perr := s.InviteByCode(ctx, userID, code)
		if perr == nil && preview.AlreadyClaimed {
			return ErrAlreadyClaimed
		}
		return ErrNotFound
	}

	return nil
}
