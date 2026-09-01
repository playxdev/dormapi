# dormapi

Backend for **dorm.place** — the multi-tenant dormitory management platform.

This service is the platform. The LINE MINI App
([playxdev/dormmini](https://github.com/playxdev/dormmini)) is one client of
it; the operator-facing web app and, later, LINE Messaging API webhooks are
others. The API contract is specified in that repository's
`docs/DESIGN-LINE-MINI.md` §11.

## Status

**Milestone 1 — authentication.** A LINE user can sign in through the MINI App
and be resolved to a property and room.

```
POST /api/v1/auth/line   { id_token }  ->  { token, expires_at }
GET  /api/v1/me                        ->  { user_id, tenant_id, property_id,
                                             property_name, room_id,
                                             display_name }
GET  /healthz                          ->  { status }
```

Invoices, payments, meter readings, repairs and announcements are not
implemented.

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
| `ADDR` | Listen address, default `:8080` |
| `ALLOWED_ORIGINS` | Comma-separated browser origins permitted to call the API |
| `LINE_CHANNEL_ID` | Numeric prefix of the LIFF ID; the `aud` every ID token must carry |
| `CLOUDFLARE_ACCOUNT_ID` | Cloudflare account holding the D1 database |
| `D1_DATABASE_ID` | D1 database UUID |
| `CLOUDFLARE_API_TOKEN` | Scoped token with D1 edit permission |
| `JWT_SECRET` | Signs session tokens; at least 32 bytes |

`ALLOWED_ORIGINS` is not optional. The MINI App is served from a different
origin, so a missing entry makes the browser block every call — and the
frontend reports that as "cannot reach the system", indistinguishable from the
API being down.

`LINE_CHANNEL_ID` changes per LINE environment. The Developing LIFF ID
`2011361700-JZlB29PM` means `LINE_CHANNEL_ID=2011361700`.

### Cloudflare API token

Create a **scoped** token with `D1:Edit` on this account only. Never a global
API key: this token can read and write every row the platform holds.

## Working with D1

D1 is designed to be reached through a Workers binding. Go cannot run on
Workers, so this service uses the REST API instead. Two properties of that,
both verified against the live API rather than assumed:

**Every query is an HTTPS round trip.** Avoid N+1 patterns. `GET /me` resolves
user, tenancy, property and room in one statement for this reason.

**`BEGIN`/`COMMIT`/`SAVEPOINT` are rejected.** D1 answers such a statement with
a message pointing at the Workers `state.storage.transaction()` API. Atomicity
instead comes from sending several statements in a single request: the batch
commits entirely or not at all. `d1.Client.Batch` is therefore the only
transaction primitive available, and any multi-step write that must not
half-apply belongs in one.

### Migrations

```bash
wrangler d1 execute dormapi-dev --remote --file ./migrations/0001_init.sql
```

Money is stored as an integer number of satang. Storing currency as a float
silently corrupts balances, and this system tracks rent, utilities and
payments.

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
internal user ID and nothing else — no property, room or tenant. Those are
resolved from the database on every request, so a revoked or moved tenancy
takes effect immediately rather than when the token expires. A client that
sends its own `property_id` is ignored.

**Errors are stable codes, not messages.** The frontend maps codes to Thai copy
of its own; a leaked SQL or LINE error would reach the user as noise. Details
go to the log.

**Logs carry no credentials** — not the `Authorization` header, not the LINE ID
token, not the session token. Every line carries a request ID so a report of
"I could not pay" can be traced.

## Layout

```
cmd/api/main.go        startup, graceful shutdown
internal/
├── config/            environment loading; reports all missing vars at once
├── d1/                Cloudflare D1 REST client, incl. atomic Batch
├── line/              LINE ID token verification
├── auth/              session token issue and verify
├── httpx/             router, middleware, handlers
└── store/             every SQL statement
migrations/
```
