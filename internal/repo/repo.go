// Package repo holds every SQL statement this service runs.
//
// The schema belongs to the backoffice (github.com/playxdev/dormplace) and
// both services read the same D1 database. Schema changes are made there, in
// its migrations directory — never here.
//
// # Why a repository layer and not queries next to the handlers
//
// The database is multi-tenant under the XYZ standard: `tenant` is the
// business renting the system, and every business table is keyed
// (tenant_id, <x>_id). PostgreSQL would catch a forgotten `WHERE tenant_id =
// ?` with row-level security. D1 has none (STANDARD §9.5), so nothing catches
// it but this file. Two rules follow, and they are the whole point of the
// package:
//
//   - Every statement touching a tenant-scoped table takes a [Tenancy] as its
//     first argument and puts `tenant_id = ?` in its WHERE clause.
//   - A [Tenancy] cannot be constructed outside this package. Its identifying
//     fields are unexported and [Repo.ResolveTenancy] is the only thing that
//     fills them, from a membership row. A tenant_id that arrived in a request
//     body, query string or header therefore cannot reach a query — there is
//     no way to put one into the type (STANDARD §9.4).
//
// The handful of statements that legitimately have no tenant yet — resolving a
// LINE identity, spending a recovery token, finding the invitation a code
// opens — are grouped under "Unscoped" below, and each says why.
//
// # Why the queries are hand written
//
// D1's batch-only atomicity and its refusal of parameters across multiple
// statements do not map onto a generator, and the query set is small.
package repo

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/playxdev/dormapi/internal/d1"
	"github.com/playxdev/dormapi/internal/pii"
	"github.com/playxdev/dormapi/internal/ulid"
)

var (
	// ErrNotFound means the row does not exist, or belongs to another tenant.
	// The two are deliberately indistinguishable: answering 403 for the second
	// confirms the id is real, which is how one tenant's session maps another
	// tenant's data (STANDARD §9.4).
	ErrNotFound = errors.New("repo: not found")

	// ErrInvalid marks input the caller got wrong.
	ErrInvalid = errors.New("repo: invalid input")

	// ErrAlreadyClaimed means the lease behind an invitation is already held by
	// someone. Invitations are single use.
	ErrAlreadyClaimed = errors.New("repo: already claimed")

	// ErrConflict marks a write that lost a race it could not win — an email
	// address taken between the check and the insert, a LINE account already
	// bound elsewhere.
	ErrConflict = errors.New("repo: conflict")
)

// Repo is the only door to the database.
type Repo struct {
	db     *d1.Client
	pepper pii.Pepper

	// backofficeURL is where the lease a resident is about to confirm is
	// rendered. Empty in a deployment without one, in which case the preview
	// carries no link rather than an address that answers nothing.
	backofficeURL string

	// lineScope is `account_identity.provider_scope` for LINE: the channel id
	// of the login channel serving this environment.
	//
	// It is part of the identity's unique key, not decoration. The same person
	// has a different LINE userId under each channel, so an identity row
	// without the channel it was issued for would collide the day a second
	// channel is added — which is exactly what adopting the MINI App channel
	// would do.
	lineScope string
}

func New(db *d1.Client, pepper pii.Pepper, lineScope, backofficeURL string) *Repo {
	return &Repo{db: db, pepper: pepper, lineScope: lineScope, backofficeURL: backofficeURL}
}

// Ping checks that the database this service was configured with is actually
// reachable. Configuration is the failure that matters: a wrong
// D1_DATABASE_ID leaves the service running and answering, with every query
// failing.
func (r *Repo) Ping(ctx context.Context) error { return r.db.Ping(ctx) }

// Account is a person. Under XYZ it carries no tenant, no role and no
// credential: those are relationships, held in `membership` and
// `account_identity` (rule 1).
type Account struct {
	ID   string
	Name string

	// Email is what makes recovery possible and nothing else does, so the app
	// has to know both whether one is on file and whether it has been proven —
	// an unverified address recovers nothing. It lives in `account_identity`,
	// never on the account row.
	Email    string
	Verified bool
}

// Tenancy is a resolved membership: this account, acting as this resident, at
// this operator, on this lease.
//
// The identifying fields are unexported on purpose — see the package comment.
// The exported ones are what a handler renders, and none of them can select a
// row.
type Tenancy struct {
	tenantID     string
	membershipID string
	partyID      string

	Account      Account
	ResidentID   string
	OperatorName string
	ContractID   string
	BuildingID   string
	BuildingName string
	RoomID       string
	RoomNumber   string
}

// ResolveTenancy answers which lease the caller may see, from their
// memberships alone.
//
// Every D1 call is an HTTPS round trip, so the whole context is one query:
// an N+1 here would be felt directly by the resident.
//
// A person may hold memberships at more than one operator — `client_tenancy_mode`
// is MULTI for this vertical, because renting a room near work and one near
// family is ordinary (VERTICAL §3). Until the app offers a switcher this
// returns the most recently started lease, ordered explicitly so the answer is
// stable rather than whatever the query planner felt like.
//
// A SUSPENDED membership still resolves. It must be able to see why it was
// suspended; what it cannot do is write, and every write path checks its own
// guard rather than relying on this one.
func (r *Repo) ResolveTenancy(ctx context.Context, accountID string) (*Tenancy, error) {
	res, err := r.db.Query(ctx, `
		SELECT m.tenant_id, m.membership_id,
		       t.name AS operator_name,
		       p.party_id, a.account_id, a.display_name,
		       ei.external_id AS email, ei.verified_at,
		       c.contract_id,
		       b.building_id, b.name AS building_name,
		       r.room_id, r.number AS room_number
		FROM membership m
		JOIN account a  ON a.account_id = m.account_id
		JOIN tenant t   ON t.tenant_id = m.tenant_id
		JOIN party p    ON p.tenant_id = m.tenant_id
		                AND p.membership_id = m.membership_id
		                AND p.deleted_at IS NULL
		JOIN contract c ON c.tenant_id = p.tenant_id AND c.party_id = p.party_id
		                AND c.status IN ('ACTIVE', 'ENDING') AND c.deleted_at IS NULL
		JOIN room r     ON r.tenant_id = c.tenant_id AND r.room_id = c.room_id
		                AND r.deleted_at IS NULL
		JOIN building b ON b.tenant_id = r.tenant_id AND b.building_id = r.building_id
		                AND b.deleted_at IS NULL
		LEFT JOIN account_identity ei ON ei.account_id = a.account_id
		                AND ei.provider = 'EMAIL' AND ei.deleted_at IS NULL
		WHERE m.account_id = ?1
		  AND m.kind = 'CLIENT'
		  AND m.status IN ('ACTIVE', 'SUSPENDED')
		  AND m.deleted_at IS NULL
		  AND t.status = 'ACTIVE' AND t.deleted_at IS NULL
		ORDER BY c.start_date DESC, c.contract_id DESC
		LIMIT 1`, accountID)
	if err != nil {
		return nil, fmt.Errorf("repo: resolve tenancy: %w", err)
	}
	if len(res.Results) == 0 {
		return nil, ErrNotFound
	}

	row := res.Results[0]
	partyID := text(row["party_id"])
	return &Tenancy{
		tenantID:     text(row["tenant_id"]),
		membershipID: text(row["membership_id"]),
		partyID:      partyID,
		Account: Account{
			ID:       text(row["account_id"]),
			Name:     text(row["display_name"]),
			Email:    text(row["email"]),
			Verified: text(row["verified_at"]) != "",
		},
		ResidentID:   partyID,
		OperatorName: text(row["operator_name"]),
		ContractID:   text(row["contract_id"]),
		BuildingID:   text(row["building_id"]),
		BuildingName: text(row["building_name"]),
		RoomID:       text(row["room_id"]),
		RoomNumber:   text(row["room_number"]),
	}, nil
}

// auditStatement appends one row to `audit_event`.
//
// Append-only (rule 9), and separate from whatever it describes because this
// service cannot write two parameterised statements atomically — see the
// package comment on D1's limits. Callers write the audit row first: a stray
// entry describing something that did not happen is a question someone can
// answer later; an action with no record of who took it is not.
func (r *Repo) audit(ctx context.Context, ev auditEvent) error {
	tenantID := any(nil)
	if ev.TenantID != "" {
		tenantID = ev.TenantID
	}
	actorAccount := any(nil)
	if ev.ActorAccountID != "" {
		actorAccount = ev.ActorAccountID
	}
	reason := any(nil)
	if ev.Reason != "" {
		reason = ev.Reason
	}
	result := ev.Result
	if result == "" {
		result = "SUCCESS"
	}

	_, err := r.db.Query(ctx, `
		INSERT INTO audit_event
			(event_id, occurred_at, tenant_id, actor_type, actor_account_id,
			 action, target_type, target_id, result, reason, request_id)
		VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?9, ?10, ?11)`,
		ulid.New(), now(), tenantID, ev.ActorType, actorAccount,
		ev.Action, ev.TargetType, ev.TargetID, result, reason, ev.RequestID)
	if err != nil {
		return fmt.Errorf("repo: audit %s: %w", ev.Action, err)
	}
	return nil
}

type auditEvent struct {
	TenantID       string // empty for a platform-level event
	ActorType      string // PLATFORM|STAFF|CLIENT|SYSTEM|ANONYMOUS
	ActorAccountID string
	Action         string
	TargetType     string
	TargetID       string
	Result         string // defaults to SUCCESS
	Reason         string
	RequestID      string
}

// now is the single source of the timestamps this service writes.
//
// Every stored time is UTC ISO-8601 created at the application layer, never by
// the database (STANDARD, schema header). Two ways of getting that wrong are
// live here:
//
// SQLite's datetime('now') writes `YYYY-MM-DD HH:MM:SS`, which has no `T` and
// no zone and sorts before every ISO-8601 string. A column holding both makes
// `expires_at > ?` answer wrongly at the boundary.
//
// Go's RFC3339Nano trims trailing zeros and writes up to nine fractional
// digits, where JavaScript's toISOString always writes exactly three. Both are
// valid ISO-8601 and they compare as text: `...12.631827377Z` sorts *before*
// `...12.631Z`, because '8' is below 'Z'. The error is sub-millisecond and
// harmless today, and the point of pinning the format is that it stays that
// way. This is what the backoffice writes.
const timeFormat = "2006-01-02T15:04:05.000Z"

func now() string { return time.Now().UTC().Format(timeFormat) }

// timestamp formats a time the same way, for the columns that carry one the
// caller chose rather than the moment of the write.
func timestamp(t time.Time) string { return t.UTC().Format(timeFormat) }

// Every list in this package orders by its id after its natural key.
//
// Two payments reported in the same second, two repairs filed in the same
// millisecond, two readings for one period across two rooms: without the
// tiebreaker SQLite is free to return them in either order, and a list that
// reorders between two requests is a list that can repeat or skip a row when
// it is paged. The ids are ULIDs, so ordering by one is ordering by time.

// isUnique reports whether a D1 error is a unique-index violation. It is the
// only error text this package matches on: the alternative is a SELECT before
// every INSERT, which does not close the race it is trying to close.
func isUnique(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
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
