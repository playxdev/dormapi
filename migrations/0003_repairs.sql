-- Repair requests and their status history.
--
-- Status lives in two places on purpose: repair_requests.status is the current
-- value the tenant sees, and repair_events is the append-only record of how it
-- got there. The history is what answers "when did you say it was fixed?".

CREATE TABLE repair_requests (
    id          TEXT PRIMARY KEY,
    property_id TEXT NOT NULL REFERENCES properties(id),
    room_id     TEXT NOT NULL REFERENCES rooms(id),
    tenancy_id  TEXT NOT NULL REFERENCES tenancies(id),

    -- Human-facing reference, e.g. R-6809001: Buddhist-era year, month, and a
    -- per-property sequence. Tenants quote this to staff.
    period      TEXT NOT NULL,
    seq         INTEGER NOT NULL,
    ref         TEXT NOT NULL,

    category    TEXT NOT NULL DEFAULT 'other',
    title       TEXT NOT NULL,
    detail      TEXT NOT NULL DEFAULT '',

    -- pending | in_progress | done | cancelled
    status      TEXT NOT NULL DEFAULT 'pending',

    created_at  TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at  TEXT NOT NULL DEFAULT (datetime('now')),

    -- The sequence is assigned by a subquery inside the INSERT, which is atomic
    -- for one statement but not against a concurrent one. This constraint turns
    -- that race into a failed insert the caller can retry, rather than two
    -- requests sharing a reference number.
    UNIQUE (property_id, period, seq)
);

CREATE TABLE repair_events (
    id         TEXT PRIMARY KEY,
    repair_id  TEXT NOT NULL REFERENCES repair_requests(id),
    status     TEXT NOT NULL,
    note       TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX idx_repairs_tenancy ON repair_requests (tenancy_id, created_at DESC);
CREATE INDEX idx_repair_events_repair ON repair_events (repair_id, created_at);
