package store

import (
	"context"
	"fmt"
)

// Announcement is a notice the owner published to a whole building.
//
// It is addressed to the building, never to a room or a person, so every
// tenant of that building reads the same text. `property_*` keeps the wire
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

// announcementSelect expects ?1 to be the user ID.
//
// Reach is derived from the caller's own active contracts, so a tenant holding
// rooms in two buildings sees both boards and a tenant with none sees nothing.
// The join can match a row once per contract, hence the GROUP BY.
//
// Drafts (published_at NULL) and notices whose day has passed are excluded
// here rather than in Go: what a tenant may read is a property of the query,
// so a caller that forgets a condition gets no rows instead of someone's
// unpublished draft.
const announcementSelect = `
	SELECT a.id, a.title, a.body, a.pinned, a.published_at, a.expires_at,
	       b.id AS property_id, b.name AS property_name,
	       (ar.user_id IS NOT NULL) AS is_read
	FROM announcements a
	JOIN buildings b  ON b.id = a.building_id
	JOIN rooms rm     ON rm.building_id = a.building_id
	JOIN contracts c  ON c.room_id = rm.id AND c.status = 'active'
	JOIN tenants t    ON t.id = c.tenant_id
	LEFT JOIN announcement_reads ar ON ar.announcement_id = a.id AND ar.user_id = ?1
	WHERE t.user_id = ?1
	  AND a.published_at IS NOT NULL
	  AND (a.expires_at IS NULL OR a.expires_at >= date('now'))`

// AnnouncementsForUser lists what the caller may read, pinned first.
func (s *Store) AnnouncementsForUser(ctx context.Context, userID string) ([]Announcement, error) {
	res, err := s.db.Query(ctx, announcementSelect+`
		GROUP BY a.id
		ORDER BY a.pinned DESC, a.published_at DESC`, userID)
	if err != nil {
		return nil, fmt.Errorf("store: list announcements: %w", err)
	}

	announcements := make([]Announcement, 0, len(res.Results))
	for _, row := range res.Results {
		announcements = append(announcements, scanAnnouncement(row))
	}
	return announcements, nil
}

func (s *Store) AnnouncementForUser(ctx context.Context, userID, announcementID string) (*Announcement, error) {
	res, err := s.db.Query(ctx, announcementSelect+`
		AND a.id = ?2
		GROUP BY a.id`, userID, announcementID)
	if err != nil {
		return nil, fmt.Errorf("store: get announcement: %w", err)
	}
	if len(res.Results) == 0 {
		return nil, ErrNotFound
	}
	announcement := scanAnnouncement(res.Results[0])
	return &announcement, nil
}

// MarkAnnouncementRead records that this tenant has opened the notice.
//
// One statement, because D1 permits no parameterised multi-statement write:
// the reader's right to the row is re-checked inside the INSERT, so nothing
// about who may mark what comes from the request.
//
// OR IGNORE makes a second open a no-op rather than an error — the app marks
// on every view, and the first read is the one worth keeping.
func (s *Store) MarkAnnouncementRead(ctx context.Context, userID, announcementID string) error {
	res, err := s.db.Query(ctx, `
		INSERT OR IGNORE INTO announcement_reads (announcement_id, user_id)
		SELECT a.id, ?1
		FROM announcements a
		JOIN rooms rm    ON rm.building_id = a.building_id
		JOIN contracts c ON c.room_id = rm.id AND c.status = 'active'
		JOIN tenants t   ON t.id = c.tenant_id
		WHERE t.user_id = ?1
		  AND a.id = ?2
		  AND a.published_at IS NOT NULL
		LIMIT 1`, userID, announcementID)
	if err != nil {
		return fmt.Errorf("store: mark announcement read: %w", err)
	}
	if res.Meta.Changes > 0 {
		return nil
	}

	// Nothing was written for one of two reasons: the row is not the caller's
	// to read, or they had already read it. Only the first is an error, and
	// telling them apart costs a query only on this rare path.
	if _, err := s.AnnouncementForUser(ctx, userID, announcementID); err != nil {
		return err
	}
	return nil
}

func scanAnnouncement(row map[string]any) Announcement {
	return Announcement{
		ID:           text(row["id"]),
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
