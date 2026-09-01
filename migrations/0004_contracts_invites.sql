-- Contracts, invites, and the terms a tenant agreed to.
--
-- A contract exists independently of whether the tenant has claimed it in the
-- app: the paper is signed and the room is active whether or not anyone has
-- scanned a QR code yet.

CREATE TABLE contracts (
    id          TEXT PRIMARY KEY,
    property_id TEXT NOT NULL REFERENCES properties(id),
    room_id     TEXT NOT NULL REFERENCES rooms(id),

    -- The occupant as written on the paper contract. Shown on the review screen
    -- so the tenant can tell they are claiming their own room, not a neighbour's.
    tenant_name TEXT NOT NULL DEFAULT '',

    rent_satang    INTEGER NOT NULL DEFAULT 0,
    deposit_satang INTEGER NOT NULL DEFAULT 0,
    start_date     TEXT NOT NULL,
    end_date       TEXT,

    -- draft: still being filled in. active: the owner has released it and its
    -- invite may be claimed. ended: tenancy over.
    status       TEXT NOT NULL DEFAULT 'draft',
    created_at   TEXT NOT NULL DEFAULT (datetime('now')),
    activated_at TEXT
);

-- One invite per contract, carrying nothing but an opaque code.
--
-- The QR encodes only this code. Putting the terms in the QR itself would hand
-- them to anyone who photographs it, and could never be revoked.
CREATE TABLE invites (
    code        TEXT PRIMARY KEY,
    contract_id TEXT NOT NULL REFERENCES contracts(id),
    expires_at  TEXT NOT NULL,
    revoked_at  TEXT,
    created_at  TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX idx_invites_contract ON invites (contract_id);
CREATE INDEX idx_contracts_room ON contracts (room_id, status);
