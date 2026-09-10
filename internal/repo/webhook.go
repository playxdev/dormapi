package repo

import (
	"context"
	"fmt"
	"time"
)

/*
Unscoped — inbound LINE.

A message arriving at the Official Account carries a LINE userId and nothing
else. Which operator it concerns is the answer, not the question, so none of
this can take a Tenancy: it is the code that produces one.

INV-03: no path here reads a tenant from the payload. The quick-reply data a
sender taps is a claim, checked by [Repo.ResolveTenancyIn] against their own
memberships before anything uses it.
*/

// Channel names the conversation a context belongs to. `conversation_context`
// is keyed (account_id, channel) so a second channel later — a web chat, say —
// does not overwrite the LINE one.
const ChannelLINE = "LINE"

// conversationTTL is the sliding window STANDARD §4.3.1 asks for. Long enough
// that a resident answering a chooser and then typing is not asked twice;
// short enough that tomorrow's message is not silently filed under yesterday's
// operator.
const conversationTTL = 30 * time.Minute

// MarkWebhookEvent records that an event has been handled, and reports whether
// this delivery is the first.
//
// LINE redelivers: on a timeout, on a non-2xx, and sometimes for no visible
// reason. Without this an "I have paid" answered once is answered three times,
// and anything the handler writes happens as many times as LINE tries.
//
// Nothing prunes the table. The standard asks for at least 24 hours and a row
// is a provider string and an id, so keeping them costs less than the cron
// that would delete them.
func (r *Repo) MarkWebhookEvent(ctx context.Context, provider, eventID string) (bool, error) {
	if eventID == "" {
		// A delivery with no id cannot be deduplicated. Treating it as fresh
		// is the safe half of the choice: the alternative is dropping real
		// events whenever LINE omits the field.
		return true, nil
	}
	res, err := r.db.Query(ctx, `
		INSERT OR IGNORE INTO webhook_event_seen (provider, event_id, processed_at)
		VALUES (?1, ?2, ?3)`, provider, eventID, now())
	if err != nil {
		return false, fmt.Errorf("repo: mark webhook event: %w", err)
	}
	return res.Meta.Changes > 0, nil
}

// Operator is one membership the sender holds, named well enough to put on a
// button.
type Operator struct {
	TenantID string
	Name     string
}

// Inbound resolves who sent a message and which operators it could concern.
//
// The list is every *occupying* CLIENT membership: INVITED, ACTIVE, SUSPENDED
// and RELEASE_PENDING. LEFT and BANNED are history — the party, the lease and
// the invoices survive them (INV-30), but they grant nothing, and offering one
// as a choice would invite a conversation that goes nowhere.
//
// An account with no LINE identity yet reports ErrNotFound: somebody messaged
// the Official Account before ever opening the app.
func (r *Repo) Inbound(ctx context.Context, subject string) (accountID string, operators []Operator, err error) {
	res, err := r.db.Query(ctx, `
		SELECT a.account_id, m.tenant_id, t.name AS tenant_name
		FROM account_identity i
		JOIN account a ON a.account_id = i.account_id
		LEFT JOIN membership m ON m.account_id = a.account_id AND m.kind = 'CLIENT'
		     AND m.status IN ('INVITED','ACTIVE','SUSPENDED','RELEASE_PENDING')
		     AND m.deleted_at IS NULL
		LEFT JOIN tenant t ON t.tenant_id = m.tenant_id
		     AND t.status = 'ACTIVE' AND t.deleted_at IS NULL
		WHERE i.provider = 'LINE' AND i.provider_scope = ?1 AND i.external_id = ?2
		  AND i.deleted_at IS NULL
		  AND a.status = 'ACTIVE' AND a.deleted_at IS NULL
		ORDER BY t.name`, r.lineScope, subject)
	if err != nil {
		return "", nil, fmt.Errorf("repo: inbound: %w", err)
	}
	if len(res.Results) == 0 {
		return "", nil, ErrNotFound
	}

	accountID = text(res.Results[0]["account_id"])
	for _, row := range res.Results {
		// The LEFT JOIN gives one row with no tenant when the account has no
		// membership at all.
		if id := text(row["tenant_id"]); id != "" && text(row["tenant_name"]) != "" {
			operators = append(operators, Operator{TenantID: id, Name: text(row["tenant_name"])})
		}
	}
	return accountID, operators, nil
}

// ConversationTenant reads which operator this chat is currently about.
//
// Inbound only. STANDARD §4.3.1 is explicit that this must never choose where
// an outgoing message goes: a notification is sent by the service that knows
// its own tenant, and using a stale chat context to address one would send a
// resident another operator's business.
func (r *Repo) ConversationTenant(ctx context.Context, accountID, channel string) (string, error) {
	res, err := r.db.Query(ctx, `
		SELECT c.tenant_id
		FROM conversation_context c
		JOIN membership m ON m.tenant_id = c.tenant_id AND m.account_id = c.account_id
		     AND m.kind = 'CLIENT'
		     AND m.status IN ('INVITED','ACTIVE','SUSPENDED','RELEASE_PENDING')
		     AND m.deleted_at IS NULL
		WHERE c.account_id = ?1 AND c.channel = ?2 AND c.expires_at > ?3`,
		accountID, channel, now())
	if err != nil {
		return "", fmt.Errorf("repo: conversation context: %w", err)
	}
	if len(res.Results) == 0 {
		return "", ErrNotFound
	}
	return text(res.Results[0]["tenant_id"]), nil
}

// RememberConversationTenant files this chat under one operator for a while.
//
// The join on membership in the read above is belt and braces for INV-34: the
// row is supposed to be deleted the moment a membership becomes LEFT or
// BANNED, and that deletion belongs to the backoffice, which owns membership
// transitions. If it is ever missed, the context stops resolving rather than
// pointing at an operator the person no longer deals with.
func (r *Repo) RememberConversationTenant(ctx context.Context, accountID, channel, tenantID string) error {
	ts := now()
	expires := timestamp(time.Now().Add(conversationTTL))
	_, err := r.db.Query(ctx, `
		INSERT INTO conversation_context (account_id, channel, tenant_id, expires_at, updated_at)
		VALUES (?1, ?2, ?3, ?4, ?5)
		ON CONFLICT (account_id, channel)
		DO UPDATE SET tenant_id = ?3, expires_at = ?4, updated_at = ?5`,
		accountID, channel, tenantID, expires, ts)
	if err != nil {
		return fmt.Errorf("repo: remember conversation: %w", err)
	}
	return nil
}

// Contact is what a resident asking for the office needs.
//
// The schema keeps no telephone number for a building, so this is the operator,
// the building and its address — which is what the notice by the lift says,
// and is enough to walk to the office or to look the number up. A phone number
// belongs on `building` before it can be answered here.
type Contact struct {
	OperatorName string
	BuildingName string
	Address      string
	RoomNumber   string
}

// Contact reads the office details for the caller's own building.
func (r *Repo) Contact(ctx context.Context, t *Tenancy) (*Contact, error) {
	res, err := r.db.Query(ctx, `
		SELECT b.name AS building_name, b.address
		FROM building b
		WHERE b.tenant_id = ?1 AND b.building_id = ?2 AND b.deleted_at IS NULL`,
		t.tenantID, t.BuildingID)
	if err != nil {
		return nil, fmt.Errorf("repo: contact: %w", err)
	}
	if len(res.Results) == 0 {
		return nil, ErrNotFound
	}
	row := res.Results[0]
	return &Contact{
		OperatorName: t.OperatorName,
		BuildingName: text(row["building_name"]),
		Address:      text(row["address"]),
		RoomNumber:   t.RoomNumber,
	}, nil
}
