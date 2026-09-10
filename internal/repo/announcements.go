package repo

import (
	"context"
	"fmt"
)

// Announcement is a notice the operator published to a whole building.
//
// It is addressed to the building, never to a room or a person, so every
// resident of that building reads the same text. `property_*` keeps the wire
// naming the MINI App already uses for what the schema calls a building.
type Announcement struct {
	ID           string `json:"id"`
	Title        string `json:"title"`
	Body         string `json:"body"`
	Pinned       bool   `json:"pinned"`
	Read         bool   `json:"read"`
	PublishedAt  string `json:"published_at"`
	ExpiresAt    string `json:"expires_at,omitempty"`
	PropertyID   string `json:"property_id"`
	PropertyName string `json:"property_name"`
}

// announcementSelect expects ?1 tenant_id, ?2 party_id, ?3 account_id.
//
// Reach is derived from the caller's own live leases, so a resident holding
// two rooms in different buildings sees both boards and one with no lease sees
// nothing. The join can match a notice once per lease, hence the GROUP BY.
//
// Drafts (published_at NULL) and notices whose day has passed are excluded
// here rather than in Go: what a resident may read is a property of the query,
// so a caller that forgets a condition gets no rows instead of somebody's
// unpublished draft.
//
// The read receipt is keyed by account, not by party: it records that this
// person opened it, and the person is the same across every operator.
const announcementSelect = `
	SELECT a.announcement_id, a.title, a.body, a.pinned,
	       a.published_at, a.expires_at,
	       b.building_id AS property_id, b.name AS property_name,
	       (ar.account_id IS NOT NULL) AS is_read
	FROM announcement a
	JOIN building b ON b.tenant_id = a.tenant_id AND b.building_id = a.building_id
	JOIN room rm    ON rm.tenant_id = a.tenant_id AND rm.building_id = a.building_id
	                AND rm.deleted_at IS NULL
	JOIN contract c ON c.tenant_id = rm.tenant_id AND c.room_id = rm.room_id
	                AND c.party_id = ?2
	                AND c.status IN ('ACTIVE', 'ENDING') AND c.deleted_at IS NULL
	LEFT JOIN announcement_read ar
	                ON ar.tenant_id = a.tenant_id
	                AND ar.announcement_id = a.announcement_id
	                AND ar.account_id = ?3
	WHERE a.tenant_id = ?1
	  AND a.deleted_at IS NULL
	  AND a.published_at IS NOT NULL
	  AND (a.expires_at IS NULL OR a.expires_at >= ?4)`

// Announcements lists what the caller may read, pinned first.
func (r *Repo) Announcements(ctx context.Context, t *Tenancy) ([]Announcement, error) {
	res, err := r.db.Query(ctx, announcementSelect+`
		GROUP BY a.announcement_id
		ORDER BY a.pinned DESC, a.published_at DESC, a.announcement_id DESC`,
		t.tenantID, t.partyID, t.Account.ID, today())
	if err != nil {
		return nil, fmt.Errorf("repo: list announcements: %w", err)
	}

	announcements := make([]Announcement, 0, len(res.Results))
	for _, row := range res.Results {
		announcements = append(announcements, scanAnnouncement(row))
	}
	return announcements, nil
}

func (r *Repo) Announcement(ctx context.Context, t *Tenancy, announcementID string) (*Announcement, error) {
	res, err := r.db.Query(ctx, announcementSelect+`
		AND a.announcement_id = ?5
		GROUP BY a.announcement_id`,
		t.tenantID, t.partyID, t.Account.ID, today(), announcementID)
	if err != nil {
		return nil, fmt.Errorf("repo: get announcement: %w", err)
	}
	if len(res.Results) == 0 {
		return nil, ErrNotFound
	}
	announcement := scanAnnouncement(res.Results[0])
	return &announcement, nil
}

// MarkAnnouncementRead records that this resident has opened the notice.
//
// One statement: the reader's right to the row is re-checked inside the
// INSERT, so nothing about who may mark what comes from the request.
//
// OR IGNORE makes a second open a no-op rather than an error — the app marks
// on every view, and the first read is the one worth keeping. Nothing is
// written when a notice is published, so announcing to a hundred rooms costs
// zero writes and one per resident who actually reads.
func (r *Repo) MarkAnnouncementRead(ctx context.Context, t *Tenancy, announcementID string) error {
	res, err := r.db.Query(ctx, `
		INSERT OR IGNORE INTO announcement_read
			(tenant_id, announcement_id, account_id, read_at)
		SELECT ?1, a.announcement_id, ?3, ?5
		FROM announcement a
		JOIN room rm    ON rm.tenant_id = a.tenant_id
		                AND rm.building_id = a.building_id
		                AND rm.deleted_at IS NULL
		JOIN contract c ON c.tenant_id = rm.tenant_id AND c.room_id = rm.room_id
		                AND c.party_id = ?2
		                AND c.status IN ('ACTIVE', 'ENDING') AND c.deleted_at IS NULL
		WHERE a.tenant_id = ?1
		  AND a.announcement_id = ?4
		  AND a.published_at IS NOT NULL
		  AND a.deleted_at IS NULL
		LIMIT 1`, t.tenantID, t.partyID, t.Account.ID, announcementID, now())
	if err != nil {
		return fmt.Errorf("repo: mark announcement read: %w", err)
	}
	if res.Meta.Changes > 0 {
		return nil
	}

	// Nothing was written for one of two reasons: the notice is not the
	// caller's to read, or they had already read it. Only the first is an
	// error, and telling them apart costs a query only on this rare path.
	if _, err := r.Announcement(ctx, t, announcementID); err != nil {
		return err
	}
	return nil
}

func scanAnnouncement(row map[string]any) Announcement {
	return Announcement{
		ID:           text(row["announcement_id"]),
		Title:        text(row["title"]),
		Body:         text(row["body"]),
		Pinned:       number(row["pinned"]) == 1,
		Read:         number(row["is_read"]) == 1,
		PublishedAt:  text(row["published_at"]),
		ExpiresAt:    text(row["expires_at"]),
		PropertyID:   text(row["property_id"]),
		PropertyName: text(row["property_name"]),
	}
}
