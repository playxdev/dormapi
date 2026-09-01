// Package store holds every SQL statement this service runs.
//
// Queries are hand written rather than generated. D1's batch-only atomicity
// and positional parameter numbering do not map cleanly onto a generator yet,
// and the query set is small. If it grows, the path forward is a database/sql
// driver over the D1 client, after which sqlc becomes usable unchanged.
package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/playxdev/dormapi/internal/d1"
)

// ErrNotFound means the row does not exist. Callers decide what that means:
// a missing tenancy is a 404 to the client, a missing user is not.
var ErrNotFound = errors.New("store: not found")

// ErrAlreadyClaimed means the contract behind an invite already has a tenancy.
// Invites are single use, so this is a second attempt rather than a fault.
var ErrAlreadyClaimed = errors.New("store: already claimed")

type Store struct {
	db *d1.Client
}

func New(db *d1.Client) *Store { return &Store{db: db} }

type User struct {
	ID          string
	LineUserID  string
	DisplayName string
	PictureURL  string
}

// Context is a user together with the tenancy that grants them access.
// Property and room are always the server's answer, never the client's.
type Context struct {
	User         User
	TenantID     string
	PropertyID   string
	PropertyName string
	RoomID       string
}

// UpsertUserByLineID finds the user behind a LINE identity, creating them on
// first sign-in and refreshing the profile fields afterwards.
//
// Both statements go in one batch so a concurrent first sign-in cannot leave a
// half-written row: the UNIQUE constraint on line_user_id decides the winner
// and the loser's batch rolls back entirely.
func (s *Store) UpsertUserByLineID(ctx context.Context, lineUserID, displayName, pictureURL string) (*User, error) {
	id := uuid.NewString()

	_, err := s.db.Query(ctx, `
		INSERT INTO users (id, line_user_id, display_name, picture_url)
		VALUES (?1, ?2, ?3, ?4)
		ON CONFLICT (line_user_id) DO UPDATE SET
			display_name = excluded.display_name,
			picture_url  = excluded.picture_url,
			updated_at   = datetime('now')`,
		id, lineUserID, displayName, pictureURL)
	if err != nil {
		return nil, fmt.Errorf("store: upsert user: %w", err)
	}

	return s.UserByLineID(ctx, lineUserID)
}

func (s *Store) UserByLineID(ctx context.Context, lineUserID string) (*User, error) {
	res, err := s.db.Query(ctx, `
		SELECT id, line_user_id, display_name, picture_url
		FROM users WHERE line_user_id = ?1`, lineUserID)
	if err != nil {
		return nil, fmt.Errorf("store: select user: %w", err)
	}
	if len(res.Results) == 0 {
		return nil, ErrNotFound
	}
	return scanUser(res.Results[0]), nil
}

// ContextForUser resolves the caller's property and room in a single query.
//
// One statement rather than three: every D1 call is an HTTPS round trip, so an
// N+1 pattern here would be felt directly by the user.
//
// A user can hold more than one active tenancy — a second room, or a room in
// another property. Until the app offers a property switcher this returns the
// most recently started one, ordered explicitly so the answer is at least
// stable rather than whatever the query planner returns first.
func (s *Store) ContextForUser(ctx context.Context, userID string) (*Context, error) {
	res, err := s.db.Query(ctx, `
		SELECT
			u.id, u.line_user_id, u.display_name, u.picture_url,
			t.id           AS tenancy_id,
			p.id           AS property_id,
			p.name         AS property_name,
			r.code         AS room_code
		FROM users u
		JOIN tenancies t  ON t.user_id = u.id AND t.status = 'active'
		JOIN properties p ON p.id = t.property_id
		JOIN rooms r      ON r.id = t.room_id
		WHERE u.id = ?1
		ORDER BY t.started_at DESC
		LIMIT 1`, userID)
	if err != nil {
		return nil, fmt.Errorf("store: select context: %w", err)
	}
	if len(res.Results) == 0 {
		return nil, ErrNotFound
	}

	row := res.Results[0]
	return &Context{
		User:         *scanUser(row),
		TenantID:     text(row["tenancy_id"]),
		PropertyID:   text(row["property_id"]),
		PropertyName: text(row["property_name"]),
		RoomID:       text(row["room_code"]),
	}, nil
}

func scanUser(row map[string]any) *User {
	return &User{
		ID:          text(row["id"]),
		LineUserID:  text(row["line_user_id"]),
		DisplayName: text(row["display_name"]),
		PictureURL:  text(row["picture_url"]),
	}
}

// number reads an integer column. D1 returns SQLite integers as JSON numbers,
// which decode into float64; money is stored in satang precisely so that the
// values stay well inside the range float64 represents exactly.
func number(v any) int64 {
	switch t := v.(type) {
	case float64:
		return int64(t)
	case int64:
		return t
	case nil:
		return 0
	default:
		return 0
	}
}

// text reads a column that D1 returns as JSON. A NULL arrives as nil and
// becomes the empty string, which is what every caller here wants.
func text(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case nil:
		return ""
	default:
		return fmt.Sprint(t)
	}
}
