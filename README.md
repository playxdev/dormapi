# dormapi

Backend for **dorm.place** — the multi-tenant dormitory management platform.

This is the resident-facing API. It is one of three services over **one shared
D1 database**, and the schema underneath it is the **XYZ multi-tenant
standard** (`../docs/XYZ_MULTI_TENANT_STANDARD.md`) plus the `DORM` vertical:

| Repo | Role |
| --- | --- |
| [playxdev/dormplace](https://github.com/playxdev/dormplace) | Backoffice. **Owns the schema and its migrations.** |
| [playxdev/dormmini](https://github.com/playxdev/dormmini) | LINE MINI App, the tenant's client |
| playxdev/dormapi | This service, serving the MINI App |

**Schema changes belong in `dormplace/migrations`, never here.** A second
database would make the onboarding flow impossible: a contract activated in the
backoffice has to be visible to the MINI App.

The wire contract keeps the names the design document uses — `property_id`,
`room_id` — even though the schema calls them buildings and room numbers.

### The word "tenant" means two things, and only one of them is here

Under XYZ a **tenant** is *the business renting the system* — one dormitory
operator. The person who rents a room is a **party** (their record with that
operator) reached through a **membership** (their right to use the app). The
pre-XYZ schema had a `tenants` table meaning the second, and this service used
the word that way throughout.

It no longer does. On the wire, `tenant_id` became `resident_id` in `GET /me`,
and the invite preview's `tenant_name` became `resident_name`.

**Deploy `mini` first, or together.** Those two fields are the only breaking
change in the move to XYZ, and a build of the app that predates it reads
`tenant_name` — against a newer API it would render the review screen with no
resident on it.

```text
ACCOUNT      one person, no tenant_id, no role, no LINE id   (global)
MEMBERSHIP   (account, tenant, kind=CLIENT) — the right to use the app
PARTY        the operator's record of a resident; leases hang off this
```

A person may hold memberships at two operators: `client_tenancy_mode` is
`MULTI` for this vertical, because renting a room near work and one near family
is ordinary. `GET /me` returns the most recently started lease until the app
offers a switcher.

## Status

**Milestone 1 — authentication.** A LINE user can sign in through the MINI App
and be resolved to a property and room.

```
POST /api/v1/auth/line          { id_token } -> { token, expires_at }
GET  /api/v1/me                 -> { user_id, resident_id, operator_name,
                                     property_id, property_name, room_id,
                                     display_name }
GET  /healthz                   -> { status }
```

**Phase 2 — tenant features.** Complete as of 1.0.0. `My Room` is covered by
`GET /me`; everything else has its own endpoint.

```
GET  /api/v1/me/invoices        -> { invoices[], outstanding_satang }
GET  /api/v1/me/invoices/{id}   -> invoice + items[] + payments[]
GET  /api/v1/me/repairs         -> { repairs[] }
POST /api/v1/me/repairs         -> ticket
GET  /api/v1/me/repairs/{id}    -> ticket
GET  /api/v1/me/invoices/{id}/payment   -> PromptPay payloads
POST /api/v1/me/invoices/{id}/payments  -> report a payment
GET  /api/v1/me/meters          -> { meters[] }
GET  /api/v1/me/announcements   -> { announcements[], unread_count }
GET  /api/v1/me/announcements/{id}      -> announcement
POST /api/v1/me/announcements/{id}/read -> marks it opened
GET  /api/v1/invites/{code}     -> terms to review before confirming
POST /api/v1/invites/{code}/claim -> binds the caller to the contract's room
```


`/me/repairs` is the tenant's name for what the schema calls a ticket. The wire
contract also keeps `property_id` and `room_id`, which the schema calls
buildings and room numbers — the MINI App and the design document both speak of
properties and rooms, and renaming a deployed contract would buy nothing.

### Meter readings

A reading belongs to a room, and a room outlives a tenancy, so each reading is
bounded by `meter_reading.contract_id` — the caller's own lease. The join on
the room alone would hand a resident who moved in last month the previous
occupant's consumption. The pre-XYZ schema had no such column and this query
compared the reading's period against the lease's dates instead; the column is
exact where the date range was an approximation of it.

Skipped rooms are left out: a walk that passed a door without reading it has no
number to show, and rendering it as zero usage would be a lie. Photos are not
exposed — they are the owner's audit trail, and what a tenant needs to check a
bill is the two numbers and the difference.

### Announcements

An announcement is addressed to a **building**, never to a room or a person:
this is the notice board by the lift, not a letter. The list is derived from the
caller's own active contracts, so a tenant renting in two buildings sees both
boards and one row per notice, and a tenant with no contract sees nothing.

Drafts and expired notices are excluded in SQL rather than in Go. What a tenant
may read is a property of the query, so a caller that forgets a condition gets
no rows instead of somebody's unpublished draft.

Read state is the **absence** of a row in `announcement_read`, keyed by
account rather than by party: it records that a person opened a notice, and the
person is the same across every operator. Publishing to a
building of 100 rooms costs zero writes, and one write lands per tenant who
actually opens a notice — which is what keeps this inside D1's write budget.
Marking is idempotent: the MINI App marks on every view, and only the first one
writes.

### Paying

Two PromptPay payloads are returned per invoice. The **full** one carries the
outstanding amount, which removes the chance of typing it wrong. The **open**
one carries no amount at all — a payload with the amount embedded cannot be
part-paid, so this is what makes instalments possible for dormitories that
allow them.

The payload builder lives in `internal/promptpay` and mirrors the backoffice's
`src/lib/promptpay.ts`. Two implementations of one wire format is a correctness
risk a bank app would surface as a QR it refuses to read, so the tests assert
byte-for-byte equality against payloads generated by that file.

Reporting a payment is a **claim, not a fact**: the money went to the owner's
bank and nothing here can observe it. The row is written `status = 'REPORTED'`
and does not count towards the invoice balance until the owner confirms it
against their statement. An idempotency key makes a retry a no-op, because a tenant on a bad
connection will tap twice and two rows would look like two transfers.

### Onboarding

An invite is an opaque single-use code standing for one contract. Reviewing it
returns the terms; claiming it binds the caller and records what they saw.

Neither step accepts terms from the client. What the tenant agreed to is copied
from the contract server-side, so a confirmation cannot be replayed with
different numbers than the ones shown.

The code itself is never stored or transmitted in the clear. The backoffice
writes `HMAC-SHA256(PII_PEPPER, normalized_code)` and this service resolves an
invitation by reproducing that hash, so `PII_PEPPER` must be **byte-identical**
in both — a mismatch presents as a QR that opens nothing, with no error
anywhere. `internal/pii` is a port of the backoffice's `hashField`, and its
tests assert against digests generated by that TypeScript.

**Single use** is enforced by one statement that can only succeed once: the
`UPDATE party ... WHERE account_id IS NULL OR account_id = <caller>` that binds
the resident to their record. The claim needs six writes and D1 gives this
service no parameterised multi-statement write, so the others are ordered
around that guard and are all safe to repeat:

```text
membership   INSERT ... WHERE NOT EXISTS          repeatable
party link   UPDATE ... account_id IS NULL OR me  THE GUARD
lease        UPDATE ... WHERE confirmed_at IS NULL
invitation   UPDATE ... WHERE used_count < max_usage
redemption   INSERT OR IGNORE  (unique per invitation+account)
consent      INSERT  (append-only, doc_type = TENANT_TERMS)
```

A resident whose first attempt failed halfway retries and finishes: their own
account still matches the guard. A second person scanning the same sheet
changes no rows and is told the room is taken.

### Money

Every amount crossing the API is an integer number of **satang**
(1 THB = 100 satang), never a float and never a pre-formatted string. The
client decides how to display it.

An invoice's balance is derived as `SUM(items) - SUM(payments)`, never stored.
A payment can therefore only be inserted, never applied by mutating a running
total — which keeps balances correct without an interactive transaction, the
one thing D1 cannot give us.

## Stack

| Concern | Choice |
| --- | --- |
| Language | Go 1.26 |
| Router | [chi](https://github.com/go-chi/chi) — plain `http.Handler`, no framework context |
| Logging | `log/slog` (stdlib), JSON in production |
| Database | Cloudflare D1 (SQLite) over the REST API |
| Session tokens | JWT HS256, `github.com/golang-jwt/jwt/v5` |
| Container | Distroless static image |

## Running locally

```bash
cp .env.example .env
# fill in the Cloudflare and JWT values
go run ./cmd/api
```

The process refuses to start if configuration is missing, and pings D1 before
listening — bad credentials fail at startup rather than reaching a tenant as a
vague error.

```bash
curl localhost:8080/healthz
```

## Configuration

| Variable | Purpose |
| --- | --- |
| `APP_ENV` | `development` or `production` (selects text vs JSON logs) |
| `ADDR` | Listen address, default `:8080`. Leave unset on managed platforms — `PORT` is used instead |
| `ALLOWED_ORIGINS` | Comma-separated browser origins permitted to call the API |
| `LINE_CHANNEL_ID` | Numeric prefix of the LIFF ID; the `aud` every ID token must carry |
| `CLOUDFLARE_ACCOUNT_ID` | Cloudflare account holding the D1 database |
| `D1_DATABASE_ID` | D1 database UUID |
| `CLOUDFLARE_API_TOKEN` | Scoped token with D1 edit permission |
| `JWT_SECRET` | Signs session tokens; at least 32 bytes |
| `PII_PEPPER` | HMAC key for invitation codes and personal data, base64. **Must equal the backoffice's** |

`ALLOWED_ORIGINS` is not optional. The MINI App is served from a different
origin, so a missing entry makes the browser block every call — and the
frontend reports that as "cannot reach the system", indistinguishable from the
API being down.

`LINE_CHANNEL_ID` changes per LINE environment. The Developing LIFF ID
`2011361700-JZlB29PM` means `LINE_CHANNEL_ID=2011361700`. It is also written to
`account_identity.provider_scope`, because the same person has a different LINE
userId under each channel and an identity row without the channel would collide
the day a second one is added.

`PII_PEPPER` is effectively permanent: rotating it invalidates every stored
hash, so every outstanding QR stops working and every phone match fails.
`DATA_MASTER_KEY` is deliberately **not** read here — this service touches no
encrypted field, and a key it does not need is a key that cannot leak from it.

### Cloudflare API token

Create a **scoped** token with `D1:Edit` on this account only. Never a global
API key: this token can read and write every row the platform holds.

## Working with D1

D1 is designed to be reached through a Workers binding. Go cannot run on
Workers, so this service uses the REST API instead. Two properties of that,
both verified against the live API rather than assumed:

**Every query is an HTTPS round trip.** Avoid N+1 patterns. `GET /me` resolves
user, tenancy, property and room in one statement for this reason.

**There are no transactions for application writes.** This is the single
constraint that shapes `internal/repo` most, and the claim flow above is what
it looks like when a write genuinely needs six statements.
 `BEGIN`/`COMMIT`/
`SAVEPOINT` are rejected outright, with a message pointing at the Workers
`state.storage.transaction()` API. Several statements in one request *are*
atomic — but the REST API refuses parameters whenever more than one statement
is sent, and the only way around that would be building SQL by concatenation.

`d1.Client.Batch` therefore rejects statements carrying parameters, which
confines it to schema and maintenance work. **Every write carrying user input
must be a single statement.** That constraint shapes the schema more than any
other: balances are derived from append-only rows rather than maintained as a
running total, and a repair's reference number is assigned by a subquery inside
its own INSERT.

All of this was established by probing the live API, not assumed.

## Testing

```bash
go test ./...
```

Two layers, because they answer different questions.

The scripted fake in `internal/d1/d1test` speaks D1's wire format and returns
answers written by the test. It proves what a statement **says** — that the
tenant is bound, that the guard is in the WHERE clause, that a resident's
report is written `REPORTED` rather than `VERIFIED`.

It cannot prove the statement is **valid**. A column that does not exist, a
CHECK the value fails, a foreign key with no parent: a fake answers all of them
as readily as it answers a correct query. So `internal/repo/livedb_test.go`
applies the backoffice's migrations to an in-process SQLite database and serves
the same wire format over it, and `integration_test.go` drives the claim, the
reads, the payment report and the whole recovery flow through it.

The schema is read from the `dormplace` checkout beside this one — never
copied here, because a copy drifts and a test passing against last month's
schema is worse than no test. Point `DORMPLACE_MIGRATIONS` at that directory to
run from elsewhere; without it those tests skip rather than invent a schema.

**What this does not cover.** `POST /api/v1/auth/line` needs an ID token only
LINE can mint, so sign-in end to end is a phone with the app on it. The tests
cover everything the token unlocks.

### Migrations

Run from the `dormplace` checkout, which owns them:

```bash
cd ../backoffice   # github.com/playxdev/dormplace
npm run db:init:remote     # wrangler d1 migrations apply dorm-db --remote
```

Use `migrations apply`, never `d1 execute --file` on a migration. Executing the
file applies the SQL but writes no row to `d1_migrations`, so the next
`migrations apply` replays history from 0001 and dies on
`table users already exists`. That is exactly what happened to `dorm-db`, and
repairing it meant inserting the already-applied names into the ledger by hand.

Money is stored as an integer number of satang throughout both services.
Storing currency as a float silently corrupts balances, and this system tracks
rent, utilities and payments.

Only **verified** payments count towards what an invoice has been paid. An
unverified slip has been submitted but not accepted; showing it as settled
would tell the tenant they owe nothing while the owner still thinks otherwise.

## Security

The service holds three secrets: the Cloudflare API token, the JWT signing
secret, and — once Phase 3 arrives — the LINE channel secret for verifying
Messaging API webhook signatures. All come from the environment; none are
committed.

**Verifying a LINE identity needs no channel secret.** The ID token is checked
with LINE's `POST /oauth2/v2.1/verify`, which authenticates the token itself.
The `aud` claim is re-checked against `LINE_CHANNEL_ID` so a token minted for
another channel cannot be replayed here.

**Authorization is never delegated to the client.** A session token carries the
account id and nothing else — no property, room, party or tenant. All of them
are resolved from `membership` on every request, so a revoked or moved tenancy
takes effect immediately rather than when the token expires. A client that
sends its own `property_id` is ignored.

**A tenant_id cannot arrive from a request.** D1 has no row-level security, so
a forgotten `WHERE tenant_id = ?` is caught by nothing at runtime. Instead,
every scoped statement takes a `repo.Tenancy` whose identifying fields are
unexported, and `ResolveTenancy` is the only thing that fills them. Outside
`internal/repo` there is no way to construct one — so there is no way to write
the query that leaks. Cross-tenant access answers **404**, never 403: a 403
would confirm the id is real.

**Errors are stable codes, not messages.** The frontend maps codes to Thai copy
of its own; a leaked SQL or LINE error would reach the user as noise. Details
go to the log.

**Logs carry no credentials** — not the `Authorization` header, not the LINE ID
token, not the session token. Every line carries a request ID so a report of
"I could not pay" can be traced.

## Deployment

The service is a single static binary in a distroless image; any container host
works.

**DigitalOcean App Platform** builds the `Dockerfile` straight from GitHub. It
injects `PORT`, so leave `ADDR` unset and let the service bind what the platform
assigns. Point the health check at `/healthz`.

Set every variable from the table above as an app-level environment variable,
marking `CLOUDFLARE_API_TOKEN` and `JWT_SECRET` as secrets. The process exits at
startup if any is missing, naming all of them at once.

## Layout

```
cmd/api/main.go        startup, graceful shutdown
internal/
├── config/            environment loading; reports all missing vars at once
├── d1/                Cloudflare D1 REST client, incl. atomic Batch
│   └── d1test/        the same wire format, from scripted answers
├── line/              LINE ID token verification
├── auth/              session token issue and verify
├── httpx/             router, middleware, handlers
├── pii/               HMAC for invitation codes; a port of the backoffice's
├── ulid/              XYZ primary keys; a port of the backoffice's
└── repo/              every SQL statement, each scoped to a resolved tenant
```

There is no `migrations/` here. The schema lives in `dormplace/migrations`.

