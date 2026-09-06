package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ErrEmailTaken reports an address already attached to another account.
//
// Kept distinct from a generic failure because the tenant can act on it: the
// address is theirs and already registered, or it is a typo for someone else's.
var ErrEmailTaken = errors.New("store: email already in use")

// SetEmail attaches an address to an account and clears any verification it
// had.
//
// Clearing is the point. An address is proof of nothing until a link sent to it
// comes back, and changing the address must not inherit the old one's proof —
// otherwise changing it to an attacker's address would inherit the right to
// recover the account.
func (s *Store) SetEmail(ctx context.Context, userID, email string) error {
	email = strings.ToLower(strings.TrimSpace(email))

	res, err := s.db.Query(ctx, `
		UPDATE users SET email = ?2, email_verified_at = NULL
		WHERE id = ?1
		  AND NOT EXISTS (SELECT 1 FROM users WHERE email = ?2 AND id <> ?1)`,
		userID, email)
	if err != nil {
		return fmt.Errorf("store: set email: %w", err)
	}
	if res.Meta.Changes == 0 {
		// Either the account is gone or the address belongs to someone else.
		// The second is the case worth reporting.
		return ErrEmailTaken
	}
	return nil
}

// IssueAuthToken records a single-use token.
//
// The plaintext never arrives here: the caller hashes it and keeps the value
// only long enough to put it in a message. A database that leaks is then a list
// of hashes rather than a set of working links.
func (s *Store) IssueAuthToken(ctx context.Context, purpose, userID, tokenHash, sentTo string, expires time.Time, issuedBy string) error {
	var by any
	if issuedBy != "" {
		by = issuedBy
	}
	_, err := s.db.Query(ctx, `
		INSERT INTO auth_tokens (id, purpose, user_id, token_hash, sent_to, expires_at, issued_by)
		VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7)`,
		uuid.NewString(), purpose, userID, tokenHash, sentTo,
		expires.UTC().Format("2006-01-02 15:04:05"), by)
	if err != nil {
		return fmt.Errorf("store: issue auth token: %w", err)
	}
	return nil
}

// ConsumeAuthToken spends a token and returns whose it was.
//
// One statement, so two requests racing the same link cannot both win: the
// update matches only while consumed_at is NULL, and RETURNING hands back the
// row it actually changed. A second attempt matches nothing and reads as
// expired, which is what it is.
func (s *Store) ConsumeAuthToken(ctx context.Context, purpose, tokenHash string) (string, error) {
	res, err := s.db.Query(ctx, `
		UPDATE auth_tokens SET consumed_at = datetime('now')
		WHERE token_hash = ?1
		  AND purpose = ?2
		  AND consumed_at IS NULL
		  AND expires_at > datetime('now')
		RETURNING user_id`, tokenHash, purpose)
	if err != nil {
		return "", fmt.Errorf("store: consume auth token: %w", err)
	}
	if len(res.Results) == 0 {
		return "", ErrNotFound
	}
	return text(res.Results[0]["user_id"]), nil
}

// MarkEmailVerified records that a link sent to the address came back.
func (s *Store) MarkEmailVerified(ctx context.Context, userID string) error {
	_, err := s.db.Query(ctx, `
		UPDATE users SET email_verified_at = datetime('now')
		WHERE id = ?1 AND email IS NOT NULL`, userID)
	if err != nil {
		return fmt.Errorf("store: mark email verified: %w", err)
	}
	return nil
}

// UserByVerifiedEmail finds the account a recovery request is for.
//
// Unverified addresses are invisible here. Anyone can type an address they do
// not own; only a verified one is evidence, and recovery is exactly the moment
// that distinction matters.
func (s *Store) UserByVerifiedEmail(ctx context.Context, email string) (string, error) {
	res, err := s.db.Query(ctx, `
		SELECT id FROM users
		WHERE email = ?1 AND email_verified_at IS NOT NULL`,
		strings.ToLower(strings.TrimSpace(email)))
	if err != nil {
		return "", fmt.Errorf("store: user by email: %w", err)
	}
	if len(res.Results) == 0 {
		return "", ErrNotFound
	}
	return text(res.Results[0]["id"]), nil
}

// LineSubjectFor reports which LINE account currently signs in as this user.
func (s *Store) LineSubjectFor(ctx context.Context, userID string) (string, error) {
	res, err := s.db.Query(ctx, `
		SELECT subject FROM identities
		WHERE provider = 'line' AND user_id = ?1`, userID)
	if err != nil {
		return "", fmt.Errorf("store: line subject: %w", err)
	}
	if len(res.Results) == 0 {
		return "", ErrNotFound
	}
	return text(res.Results[0]["subject"]), nil
}

// RebindLine points an existing account at a new LINE account.
//
// The tenant, the contracts and the payment history do not move: this changes
// only which credential opens the door, which is the whole difference between
// recovery and starting again.
//
// The audit row is written first. A stray audit entry describing a rebind that
// did not happen is a question someone can answer later; a rebind with no
// record of who had the account before is not.
func (s *Store) RebindLine(ctx context.Context, userID, oldSubject, newSubject, action, approvedBy, requestID string) error {
	var by any
	if approvedBy != "" {
		by = approvedBy
	}
	if _, err := s.db.Query(ctx, `
		INSERT INTO identity_audit_logs
			(id, user_id, action, provider, old_subject, new_subject, approved_by, request_id)
		VALUES (?1, ?2, ?3, 'line', ?4, ?5, ?6, ?7)`,
		uuid.NewString(), userID, action, oldSubject, newSubject, by, requestID); err != nil {
		return fmt.Errorf("store: audit rebind: %w", err)
	}

	// One statement: identities is keyed on (provider, subject), so moving the
	// subject is the rebind. The guard on user_id keeps it from rewriting
	// somebody else's row if the caller passes an id it should not have.
	res, err := s.db.Query(ctx, `
		UPDATE identities SET subject = ?2
		WHERE provider = 'line' AND user_id = ?1`, userID, newSubject)
	if err != nil {
		return fmt.Errorf("store: rebind line: %w", err)
	}
	if res.Meta.Changes == 0 {
		return ErrNotFound
	}
	return nil
}
