-- Invoices, line items and payments.
--
-- Every amount is an integer number of satang (1 THB = 100 satang). Floats
-- silently corrupt balances and this table tracks money owed and paid.
--
-- Balance is never stored. It is derived as SUM(items) - SUM(payments) so that
-- a payment can only ever be inserted, never applied by mutating a running
-- total. That matters here because D1 has no interactive transactions: an
-- append-only design stays correct without one.

CREATE TABLE invoices (
    id          TEXT PRIMARY KEY,
    property_id TEXT NOT NULL REFERENCES properties(id),
    room_id     TEXT NOT NULL REFERENCES rooms(id),
    tenancy_id  TEXT NOT NULL REFERENCES tenancies(id),

    -- Billing period as 'YYYY-MM'. Stored in the Gregorian calendar; the
    -- Buddhist-era year the tenant sees is a presentation concern.
    period      TEXT NOT NULL,
    due_date    TEXT NOT NULL,
    issued_at   TEXT NOT NULL DEFAULT (datetime('now')),

    -- open: awaiting payment. void: cancelled, excluded from balances.
    -- There is deliberately no 'paid' status: whether an invoice is settled is
    -- decided by its payments, so the two can never disagree.
    status      TEXT NOT NULL DEFAULT 'open',

    UNIQUE (tenancy_id, period)
);

CREATE TABLE invoice_items (
    id            TEXT PRIMARY KEY,
    invoice_id    TEXT NOT NULL REFERENCES invoices(id),
    kind          TEXT NOT NULL,
    description   TEXT NOT NULL,
    amount_satang INTEGER NOT NULL,
    sort_order    INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE payments (
    id            TEXT PRIMARY KEY,
    invoice_id    TEXT NOT NULL REFERENCES invoices(id),
    amount_satang INTEGER NOT NULL,
    paid_at       TEXT NOT NULL DEFAULT (datetime('now')),
    method        TEXT NOT NULL DEFAULT 'transfer',
    reference     TEXT NOT NULL DEFAULT '',

    -- Makes a retried payment a no-op rather than a double charge. Networks
    -- are unreliable and a tenant will tap twice.
    idempotency_key TEXT NOT NULL UNIQUE
);

CREATE INDEX idx_invoices_tenancy ON invoices (tenancy_id, period DESC);
CREATE INDEX idx_invoice_items_invoice ON invoice_items (invoice_id, sort_order);
CREATE INDEX idx_payments_invoice ON payments (invoice_id);
