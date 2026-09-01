-- Development seed. Never run against production.
--
-- Creates one property and one room matching the UX concept. A tenancy cannot
-- be seeded here because it needs a real user row, which only exists after a
-- LINE sign-in. Link a tenancy afterwards with:
--
--   INSERT INTO tenancies (id, user_id, room_id, property_id)
--   SELECT 'tn_dev_1', id, 'rm_a203', 'prop_oscar' FROM users
--   WHERE line_user_id = '<your LINE user id>';

INSERT OR IGNORE INTO properties (id, name) VALUES ('prop_oscar', 'Oscar Apartment');
INSERT OR IGNORE INTO rooms (id, property_id, code) VALUES ('rm_a203', 'prop_oscar', 'A-203');
