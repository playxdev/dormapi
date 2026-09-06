// Package store holds every SQL statement this service runs.
//
// The schema belongs to the backoffice (github.com/playxdev/dormplace) and both
// services read the same D1 database. Schema changes are made there, in its
// migrations directory — never here.
//
// Queries are hand written. D1's batch-only atomicity and its refusal of
// parameters across multiple statements do not map onto a generator, and the
// query set is small.
package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/playxdev/dormapi/internal/d1"
)

var (
	// ErrNotFound means the row does not exist, or does not belong to the
	// caller. The two are deliberately indistinguishable to clients.
	ErrNotFound = errors.New("store: not found")

	// ErrInvalid marks input the caller got wrong.
	ErrInvalid = errors.New("store: invalid input")

	// ErrAlreadyClaimed means the contract behind an invite already has a
	// confirmed tenant. Invites are single use.
	ErrAlreadyClaimed = errors.New("store: already claimed")
)

// lineNamespace derives a stable user ID from a LINE subject, so that creating
// an account is idempotent. D1 allows no parameterised multi-statement write,
// so the user row and its identity row are inserted separately; deterministic
// IDs make a retry after a partial failure converge rather than duplicate.
var lineNamespace = uuid.MustParse("6f9619ff-8b86-d011-b42d-00c04fc964ff")

type Store struct {
	db *d1.Client

	// backofficeURL is where the lease a tenant is about to confirm is
	// rendered. Empty in a deployment without one, in which case the preview
	// simply carries no link rather than an address that answers nothing.
	backofficeURL string
}

func New(db *d1.Client, backofficeURL string) *Store {
	return &Store{db: db, backofficeURL: backofficeURL}
}

// Ping checks that the database this service was configured with is actually
// reachable. Configuration is the failure that matters: a wrong
// D1_DATABASE_ID leaves the service running and answering, with every query
// failing.
func (s *Store) Ping(ctx context.Context) error { return s.db.Ping(ctx) }

type User struct {
	ID   string
	Name string
	// Email is what makes recovery possible and nothing else does, so the
	// app has to know both whether one is on file and whether it has been
	// proven — an unverified address recovers nothing.
	Email    string
	Verified bool
}

// Context is a user together with the contract that grants them access.
// Building and room are always the server's answer, never the client's.
type Context struct {
	User         User
	TenantID     string
	ContractID   string
	BuildingID   string
	BuildingName string
	RoomNumber   string
}

// UserByLineSubject resolves the account behind a LINE identity, creating it on
// first sign-in.
//
// The account is a `users` row with role 'tenant'. It is the same table the
// backoffice uses for owners and staff: a person is one account, and what they
// may do is a relationship (a membership, or a contract), not an attribute.
func (s *Store) UserByLineSubject(ctx context.Context, subject, displayName string) (*User, error) {
	found, err := s.userByIdentity(ctx, subject)
	if err == nil {
		return found, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return nil, err
	}

	id := uuid.NewSHA1(lineNamespace, []byte("line:"+subject)).String()
	name := displayName
	if name == "" {
		name = "ผู้ใช้ LINE"
	}

	// Both inserts ignore conflicts, so a retry after a partial failure lands
	// in the same place rather than creating a second account.
	if _, err := s.db.Query(ctx,
		`INSERT OR IGNORE INTO users (id, name, role) VALUES (?1, ?2, 'tenant')`,
		id, name); err != nil {
		return nil, fmt.Errorf("store: create user: %w", err)
	}
	if _, err := s.db.Query(ctx,
		`INSERT OR IGNORE INTO identities (provider, subject, user_id) VALUES ('line', ?1, ?2)`,
		subject, id); err != nil {
		return nil, fmt.Errorf("store: link identity: %w", err)
	}

	// Read back through the identity rather than trusting the ID just built:
	// if this subject was already linked to a different account, that link
	// wins.
	return s.userByIdentity(ctx, subject)
}

func (s *Store) userByIdentity(ctx context.Context, subject string) (*User, error) {
	res, err := s.db.Query(ctx, `
		SELECT u.id, u.name
		FROM identities i
		JOIN users u ON u.id = i.user_id
		WHERE i.provider = 'line' AND i.subject = ?1`, subject)
	if err != nil {
		return nil, fmt.Errorf("store: select identity: %w", err)
	}
	if len(res.Results) == 0 {
		return nil, ErrNotFound
	}
	row := res.Results[0]
	return &User{ID: text(row["id"]), Name: text(row["name"])}, nil
}

// ContextForUser resolves the caller's building and room in one query.
//
// Every D1 call is an HTTPS round trip, so an N+1 pattern here would be felt
// directly by the tenant.
//
// A person can hold more than one active contract — a second room, or a room in
// another building. Until the app offers a switcher this returns the most
// recently started one, ordered explicitly so the answer is stable.
//
// The account reaches its tenant record through the contract it confirmed,
// rather than through a stored tenants.user_id. The two said the same thing,
// and keeping both meant the claim had to write twice with no way to make the
// pair atomic.
func (s *Store) ContextForUser(ctx context.Context, userID string) (*Context, error) {
	res, err := s.db.Query(ctx, `
		SELECT u.id, u.name, u.email, u.email_verified_at,
		       t.id AS tenant_id,
		       c.id AS contract_id,
		       b.id AS building_id, b.name AS building_name,
		       r.number AS room_number
		FROM users u
		JOIN contracts c ON c.confirmed_by_user_id = u.id AND c.status = 'active'
		JOIN tenants t   ON t.id = c.tenant_id
		JOIN rooms r     ON r.id = c.room_id
		JOIN buildings b ON b.id = r.building_id
		WHERE u.id = ?1
		ORDER BY c.start_date DESC
		LIMIT 1`, userID)
	if err != nil {
		return nil, fmt.Errorf("store: select context: %w", err)
	}
	if len(res.Results) == 0 {
		return nil, ErrNotFound
	}

	row := res.Results[0]
	return &Context{
		User: User{
			ID:       text(row["id"]),
			Name:     text(row["name"]),
			Email:    text(row["email"]),
			Verified: text(row["email_verified_at"]) != "",
		},
		TenantID:     text(row["tenant_id"]),
		ContractID:   text(row["contract_id"]),
		BuildingID:   text(row["building_id"]),
		BuildingName: text(row["building_name"]),
		RoomNumber:   text(row["room_number"]),
	}, nil
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

// number reads an integer column. D1 returns SQLite integers as JSON numbers,
// which decode into float64; money is stored in satang precisely so the values
// stay well inside the range float64 represents exactly.
func number(v any) int64 {
	switch t := v.(type) {
	case float64:
		return int64(t)
	case int64:
		return t
	default:
		return 0
	}
}
