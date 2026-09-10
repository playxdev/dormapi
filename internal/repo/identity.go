package repo

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/playxdev/dormapi/internal/ulid"
)

// ErrEmailTaken reports an address already attached to another account.
//
// Kept distinct from a generic failure because the resident can act on it: the
// address is theirs and already registered, or it is a typo for someone else's.
var ErrEmailTaken = errors.New("repo: email already in use")

/*
Unscoped — identity.

`account` and `account_identity` are global tables: a person is one account
across every operator they rent from (rule 1). There is no tenant to scope
them by, and inventing one would be the bug, not the safeguard. What they can
reach once signed in is decided by [Repo.ResolveTenancy], from memberships.
*/

// AccountByLine resolves the account behind a LINE identity, creating it on
// first sign-in.
//
// Idempotence is the whole difficulty. D1 gives this service no parameterised
// multi-statement write (see the package comment), so the account row and its
// identity row are two calls, and a failure between them must not leave a
// second account behind on the retry. The order below makes the identity row
// the authority:
//
//  1. Look the identity up. Every sign-in after the first stops here, at one
//     round trip.
//  2. Insert an account with a fresh id.
//  3. INSERT OR IGNORE the identity. `ux_identity_external` is unique, so if
//     another account already holds this LINE user — a rebind, or a racing
//     first sign-in — this writes nothing.
//  4. Read back through the identity. Whoever holds it wins, not the id just
//     minted.
//  5. If the winner is somebody else, delete the account from step 2 while it
//     still has no identity pointing at it.
//
// Step 5 is best effort. An account row that survives it references nothing
// and grants nothing; a duplicated person would have been much worse.
func (r *Repo) AccountByLine(ctx context.Context, subject, displayName string) (*Account, error) {
	found, err := r.accountByLineSubject(ctx, subject)
	if err == nil {
		return found, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return nil, err
	}

	accountID := ulid.New()
	name := strings.TrimSpace(displayName)
	if name == "" {
		name = "ผู้ใช้ LINE"
	}
	ts := now()

	if _, err := r.db.Query(ctx, `
		INSERT INTO account (account_id, display_name, locale, status, created_at, updated_at)
		VALUES (?1, ?2, 'th', 'ACTIVE', ?3, ?3)`, accountID, name, ts); err != nil {
		return nil, fmt.Errorf("repo: create account: %w", err)
	}

	if _, err := r.db.Query(ctx, `
		INSERT OR IGNORE INTO account_identity
			(identity_id, account_id, provider, provider_scope, external_id,
			 verified_at, last_verified_at, is_primary, created_at)
		VALUES (?1, ?2, 'LINE', ?3, ?4, ?5, ?5, 1, ?5)`,
		ulid.New(), accountID, r.lineScope, subject, ts); err != nil {
		return nil, fmt.Errorf("repo: link line identity: %w", err)
	}

	account, err := r.accountByLineSubject(ctx, subject)
	if err != nil {
		return nil, err
	}
	if account.ID != accountID {
		// Somebody else holds this LINE user. Take back the row from step 2,
		// but only while nothing references it.
		if _, err := r.db.Query(ctx, `
			DELETE FROM account
			WHERE account_id = ?1
			  AND NOT EXISTS (SELECT 1 FROM account_identity WHERE account_id = ?1)
			  AND NOT EXISTS (SELECT 1 FROM membership WHERE account_id = ?1)`,
			accountID); err != nil {
			return nil, fmt.Errorf("repo: discard unused account: %w", err)
		}
	}

	// LINE display names change. Keeping ours in step costs one write on a
	// path that already writes, and a stale name is what the resident sees on
	// every screen.
	if account.Name != name && displayName != "" {
		if _, err := r.db.Query(ctx, `
			UPDATE account SET display_name = ?2, updated_at = ?3 WHERE account_id = ?1`,
			account.ID, name, now()); err == nil {
			account.Name = name
		}
	}
	return account, nil
}

func (r *Repo) accountByLineSubject(ctx context.Context, subject string) (*Account, error) {
	res, err := r.db.Query(ctx, `
		SELECT a.account_id, a.display_name,
		       ei.external_id AS email, ei.verified_at
		FROM account_identity i
		JOIN account a ON a.account_id = i.account_id
		LEFT JOIN account_identity ei ON ei.account_id = a.account_id
		     AND ei.provider = 'EMAIL' AND ei.deleted_at IS NULL
		WHERE i.provider = 'LINE' AND i.provider_scope = ?1 AND i.external_id = ?2
		  AND i.deleted_at IS NULL
		  AND a.status = 'ACTIVE' AND a.deleted_at IS NULL`, r.lineScope, subject)
	if err != nil {
		return nil, fmt.Errorf("repo: select line identity: %w", err)
	}
	if len(res.Results) == 0 {
		return nil, ErrNotFound
	}
	row := res.Results[0]
	return &Account{
		ID:       text(row["account_id"]),
		Name:     text(row["display_name"]),
		Email:    text(row["email"]),
		Verified: text(row["verified_at"]) != "",
	}, nil
}

// SetEmail attaches an address to an account and clears any verification it
// had.
//
// Clearing is the point. An address is proof of nothing until a link sent to
// it comes back, and a changed address must not inherit the old one's proof —
// otherwise changing it to an attacker's address would inherit the right to
// recover the account.
//
// The address lives in `account_identity`, not on the account: under XYZ an
// account has no contact column at all (rule 1), and an address is one more
// way to sign in.
func (r *Repo) SetEmail(ctx context.Context, accountID, email string) error {
	email = strings.ToLower(strings.TrimSpace(email))
	ts := now()

	res, err := r.db.Query(ctx, `
		SELECT identity_id, account_id FROM account_identity
		WHERE provider = 'EMAIL' AND provider_scope = '_' AND external_id = ?1
		  AND deleted_at IS NULL`, email)
	if err != nil {
		return fmt.Errorf("repo: lookup email: %w", err)
	}
	if len(res.Results) > 0 {
		if text(res.Results[0]["account_id"]) != accountID {
			return ErrEmailTaken
		}
		// Already theirs. Re-submitting it is how a resident asks for another
		// verification mail, so drop the proof and let the caller send one.
		if _, err := r.db.Query(ctx, `
			UPDATE account_identity SET verified_at = NULL
			WHERE identity_id = ?1`, text(res.Results[0]["identity_id"])); err != nil {
			return fmt.Errorf("repo: reset email verification: %w", err)
		}
		return nil
	}

	// The new row goes in before the old one is retired. The reverse order
	// would leave an account with no address at all if the insert then failed
	// on a unique index, and the address is the only way back into a lost
	// account.
	if _, err := r.db.Query(ctx, `
		INSERT INTO account_identity
			(identity_id, account_id, provider, provider_scope, external_id,
			 verified_at, is_primary, created_at)
		VALUES (?1, ?2, 'EMAIL', '_', ?3, NULL, 0, ?4)`,
		ulid.New(), accountID, email, ts); err != nil {
		if isUnique(err) {
			return ErrEmailTaken
		}
		return fmt.Errorf("repo: set email: %w", err)
	}

	if _, err := r.db.Query(ctx, `
		UPDATE account_identity SET deleted_at = ?3
		WHERE account_id = ?1 AND provider = 'EMAIL' AND external_id <> ?2
		  AND deleted_at IS NULL`, accountID, email, ts); err != nil {
		// The new address is saved and usable. An old row left behind resolves
		// to the same account, so recovery still lands in the right place.
		return nil
	}
	return nil
}

// MarkEmailVerified records that a link sent to the address came back.
func (r *Repo) MarkEmailVerified(ctx context.Context, accountID, address string) error {
	_, err := r.db.Query(ctx, `
		UPDATE account_identity SET verified_at = ?3, last_verified_at = ?3
		WHERE account_id = ?1 AND provider = 'EMAIL' AND external_id = ?2
		  AND deleted_at IS NULL`, accountID, strings.ToLower(strings.TrimSpace(address)), now())
	if err != nil {
		return fmt.Errorf("repo: mark email verified: %w", err)
	}
	return nil
}

// AccountByVerifiedEmail finds the account a recovery request is for.
//
// Unverified addresses are invisible here. Anyone can type an address they do
// not own; only a verified one is evidence, and recovery is exactly the moment
// that distinction matters.
func (r *Repo) AccountByVerifiedEmail(ctx context.Context, email string) (string, error) {
	res, err := r.db.Query(ctx, `
		SELECT i.account_id FROM account_identity i
		JOIN account a ON a.account_id = i.account_id
		WHERE i.provider = 'EMAIL' AND i.external_id = ?1
		  AND i.verified_at IS NOT NULL AND i.deleted_at IS NULL
		  AND a.status = 'ACTIVE' AND a.deleted_at IS NULL`,
		strings.ToLower(strings.TrimSpace(email)))
	if err != nil {
		return "", fmt.Errorf("repo: account by email: %w", err)
	}
	if len(res.Results) == 0 {
		return "", ErrNotFound
	}
	return text(res.Results[0]["account_id"]), nil
}

// LineSubjectFor reports which LINE account currently signs in as this
// account, on this channel.
func (r *Repo) LineSubjectFor(ctx context.Context, accountID string) (string, error) {
	res, err := r.db.Query(ctx, `
		SELECT external_id FROM account_identity
		WHERE provider = 'LINE' AND provider_scope = ?1 AND account_id = ?2
		  AND deleted_at IS NULL`, r.lineScope, accountID)
	if err != nil {
		return "", fmt.Errorf("repo: line subject: %w", err)
	}
	if len(res.Results) == 0 {
		return "", ErrNotFound
	}
	return text(res.Results[0]["external_id"]), nil
}

// RebindLine points an existing account at a new LINE account.
//
// The memberships, the party, the lease, the invoices and the payment history
// do not move: this changes only which credential opens the door, which is the
// whole difference between recovery and starting again.
//
// The audit row is written first, and to `audit_event` — the pre-XYZ
// `identity_audit_logs` table is gone, and the core's append-only log is where
// this belongs (rule 8).
func (r *Repo) RebindLine(ctx context.Context, accountID, oldSubject, newSubject, requestID string) error {
	if err := r.audit(ctx, auditEvent{
		ActorType:      "CLIENT",
		ActorAccountID: accountID,
		Action:         "identity.rebound",
		TargetType:     "account",
		TargetID:       accountID,
		Reason:         "line_rebind from " + redactSubject(oldSubject) + " to " + redactSubject(newSubject),
		RequestID:      requestID,
	}); err != nil {
		return err
	}

	ts := now()
	res, err := r.db.Query(ctx, `
		UPDATE account_identity
		   SET external_id = ?3, verified_at = ?4, last_verified_at = ?4
		 WHERE provider = 'LINE' AND provider_scope = ?1 AND account_id = ?2
		   AND deleted_at IS NULL`, r.lineScope, accountID, newSubject, ts)
	if err != nil {
		if isUnique(err) {
			// The new LINE account already signs in as somebody else. Moving
			// it would take that person's account away from them.
			return ErrConflict
		}
		return fmt.Errorf("repo: rebind line: %w", err)
	}
	if res.Meta.Changes > 0 {
		return nil
	}

	// No LINE identity to move: the account was reached by email alone, or the
	// row was removed. Recovery still has to end with a way in, so make one.
	if _, err := r.db.Query(ctx, `
		INSERT INTO account_identity
			(identity_id, account_id, provider, provider_scope, external_id,
			 verified_at, last_verified_at, is_primary, created_at)
		VALUES (?1, ?2, 'LINE', ?3, ?4, ?5, ?5, 1, ?5)`,
		ulid.New(), accountID, r.lineScope, newSubject, ts); err != nil {
		if isUnique(err) {
			return ErrConflict
		}
		return fmt.Errorf("repo: bind line: %w", err)
	}
	return nil
}

// redactSubject keeps enough of a LINE userId to match two audit rows to each
// other, and not enough to identify the account from the log alone.
func redactSubject(subject string) string {
	if subject == "" {
		return "(none)"
	}
	if len(subject) <= 8 {
		return "…"
	}
	return subject[:6] + "…" + subject[len(subject)-4:]
}

/*
Unscoped — single-use tokens.

`auth_token` is global for the same reason `account` is: a link mailed to a
person is about the person, not about one operator's tenancy.
*/

// IssueAuthToken records a single-use token.
//
// The plaintext never arrives here: the caller hashes it and keeps the value
// only long enough to put it in a message. A database that leaks is then a
// list of hashes rather than a set of working links.
func (r *Repo) IssueAuthToken(ctx context.Context, purpose, accountID, tokenHash, sentTo string, expires time.Time, issuedBy string) error {
	by := any(nil)
	if issuedBy != "" {
		by = issuedBy
	}
	_, err := r.db.Query(ctx, `
		INSERT INTO auth_token
			(token_id, purpose, account_id, token_hash, sent_to, expires_at, issued_by, created_at)
		VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8)`,
		ulid.New(), purpose, accountID, tokenHash, sentTo,
		timestamp(expires), by, now())
	if err != nil {
		return fmt.Errorf("repo: issue auth token: %w", err)
	}
	return nil
}

// ConsumeAuthToken spends a token and returns whose it was, with the address
// it was sent to.
//
// One statement, so two requests racing the same link cannot both win: the
// update matches only while consumed_at is NULL, and RETURNING hands back the
// row it actually changed. A second attempt matches nothing and reads as
// expired, which is what it is.
//
// `sent_to` comes back because a later change of address must not redirect a
// link already in flight — the verification marks the address the mail went
// to, not whatever is on the account when it is opened.
func (r *Repo) ConsumeAuthToken(ctx context.Context, purpose, tokenHash string) (accountID, sentTo string, err error) {
	res, err := r.db.Query(ctx, `
		UPDATE auth_token SET consumed_at = ?3
		WHERE token_hash = ?1
		  AND purpose = ?2
		  AND consumed_at IS NULL
		  AND expires_at > ?3
		RETURNING account_id, sent_to`, tokenHash, purpose, now())
	if err != nil {
		return "", "", fmt.Errorf("repo: consume auth token: %w", err)
	}
	if len(res.Results) == 0 {
		return "", "", ErrNotFound
	}
	row := res.Results[0]
	return text(row["account_id"]), text(row["sent_to"]), nil
}
