-- What the tenant actually saw and agreed to when they confirmed.
--
-- These are a snapshot, not a reference. The contract can be amended later; the
-- snapshot must not move, because it is the answer to "what rent did I agree
-- to?" months after the fact.

ALTER TABLE tenancies ADD COLUMN contract_id TEXT REFERENCES contracts(id);
ALTER TABLE tenancies ADD COLUMN agreed_rent_satang INTEGER NOT NULL DEFAULT 0;
ALTER TABLE tenancies ADD COLUMN agreed_deposit_satang INTEGER NOT NULL DEFAULT 0;
ALTER TABLE tenancies ADD COLUMN agreed_start_date TEXT NOT NULL DEFAULT '';
ALTER TABLE tenancies ADD COLUMN confirmed_at TEXT;

-- Makes an invite single-use without a second statement: claiming inserts a
-- tenancy for the contract, and a second claim collides here. D1 allows no
-- parameterised multi-statement write, so the constraint does the work that a
-- transaction would elsewhere. SQLite permits many NULLs, so tenancies created
-- before contracts existed are unaffected.
CREATE UNIQUE INDEX idx_tenancies_contract ON tenancies (contract_id);
