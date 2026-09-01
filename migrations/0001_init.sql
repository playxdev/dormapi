-- Initial schema for dorm.place.
--
-- Money is stored as an integer number of satang (1 THB = 100 satang). Storing
-- currency as a float silently corrupts balances, and this system tracks rent,
-- utilities and payments.

CREATE TABLE properties (
    id          TEXT PRIMARY KEY,
    name        TEXT NOT NULL,
    created_at  TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE rooms (
    id          TEXT PRIMARY KEY,
    property_id TEXT NOT NULL REFERENCES properties(id),
    code        TEXT NOT NULL,
    created_at  TEXT NOT NULL DEFAULT (datetime('now')),
    UNIQUE (property_id, code)
);

-- A user is a person as this platform knows them. line_user_id is the LINE
-- identity; the two are kept separate so a user can later gain other sign-in
-- methods without changing their identity here.
CREATE TABLE users (
    id            TEXT PRIMARY KEY,
    line_user_id  TEXT NOT NULL UNIQUE,
    display_name  TEXT NOT NULL DEFAULT '',
    picture_url   TEXT NOT NULL DEFAULT '',
    created_at    TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at    TEXT NOT NULL DEFAULT (datetime('now'))
);

-- A tenancy is one user occupying one room. It is the only thing that grants a
-- user access to a property's data, and the server derives it on every
-- request rather than trusting the client.
CREATE TABLE tenancies (
    id          TEXT PRIMARY KEY,
    user_id     TEXT NOT NULL REFERENCES users(id),
    room_id     TEXT NOT NULL REFERENCES rooms(id),
    property_id TEXT NOT NULL REFERENCES properties(id),
    status      TEXT NOT NULL DEFAULT 'active',
    started_at  TEXT NOT NULL DEFAULT (datetime('now')),
    ended_at    TEXT
);

CREATE INDEX idx_tenancies_user_active ON tenancies (user_id, status);
CREATE INDEX idx_rooms_property ON rooms (property_id);
