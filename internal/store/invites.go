package store

import (
	"context"
	"crypto/rand"
	"fmt"
	"strings"

	"github.com/google/uuid"
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
	PropertyName   string `json:"property_name"`
	RoomCode       string `json:"room_code"`
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
		SELECT i.code, p.name AS property_name, r.code AS room_code,
		       c.tenant_name, c.rent_satang, c.deposit_satang, c.start_date,
		       t.id AS tenancy_id, t.user_id AS tenancy_user_id
		FROM invites i
		JOIN contracts c  ON c.id = i.contract_id
		JOIN properties p ON p.id = c.property_id
		JOIN rooms r      ON r.id = c.room_id
		LEFT JOIN tenancies t ON t.contract_id = c.id
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
	claimedBy := text(row["tenancy_user_id"])
	return &InvitePreview{
		Code:           text(row["code"]),
		PropertyName:   text(row["property_name"]),
		RoomCode:       text(row["room_code"]),
		TenantName:     text(row["tenant_name"]),
		RentSatang:     number(row["rent_satang"]),
		DepositSatang:  number(row["deposit_satang"]),
		StartDate:      text(row["start_date"]),
		AlreadyClaimed: claimedBy != "",
		ClaimedBySelf:  claimedBy != "" && claimedBy == userID,
	}, nil
}

// ClaimInvite binds the caller to the contract's room and records the terms
// they confirmed.
//
// One statement, because D1 permits no parameterised multi-statement write. The
// invite is not marked used; single use is enforced by the unique index on
// tenancies.contract_id, so a second claim collides instead of succeeding.
//
// The agreed_* columns copy the contract's values at this moment rather than
// referencing them. If the owner later amends the rent, the tenant's record of
// what they agreed to must not move with it.
func (s *Store) ClaimInvite(ctx context.Context, userID, code string) error {
	id := uuid.NewString()

	res, err := s.db.Query(ctx, `
		INSERT INTO tenancies
			(id, user_id, room_id, property_id, status, contract_id,
			 agreed_rent_satang, agreed_deposit_satang, agreed_start_date, confirmed_at)
		SELECT ?1, ?2, c.room_id, c.property_id, 'active', c.id,
		       c.rent_satang, c.deposit_satang, c.start_date, datetime('now')
		FROM invites i
		JOIN contracts c ON c.id = i.contract_id
		WHERE i.code = ?3
		  AND i.revoked_at IS NULL
		  AND i.expires_at > datetime('now')
		  AND c.status = 'active'`, id, userID, code)
	if err != nil {
		// A unique-index violation means someone already claimed this contract.
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return ErrAlreadyClaimed
		}
		return fmt.Errorf("store: claim invite: %w", err)
	}
	if res.Meta.Changes == 0 {
		// The SELECT matched nothing: unknown, expired, revoked, or the
		// contract is not active.
		return ErrNotFound
	}
	return nil
}
