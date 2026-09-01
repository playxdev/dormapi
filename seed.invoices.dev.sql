-- Development seed for invoices, matching the UX concept:
-- September 2568 (2025-09), 5,250.00 THB outstanding, due 5 Sep.
--
-- Amounts are satang: 4,500.00 THB = 450000.

INSERT OR IGNORE INTO invoices (id, property_id, room_id, tenancy_id, period, due_date, status)
VALUES ('inv_2025_09', 'prop_oscar', 'rm_a203', 'tn_dev_1', '2025-09', '2025-09-05', 'open');

INSERT OR IGNORE INTO invoice_items (id, invoice_id, kind, description, amount_satang, sort_order) VALUES
    ('it_2025_09_rent',  'inv_2025_09', 'rent',        'ค่าเช่า', 450000, 1),
    ('it_2025_09_water', 'inv_2025_09', 'water',       'ค่าน้ำ',   30000, 2),
    ('it_2025_09_elec',  'inv_2025_09', 'electricity', 'ค่าไฟ',    45000, 3);

-- August, fully paid, so the history tab has something in it.
INSERT OR IGNORE INTO invoices (id, property_id, room_id, tenancy_id, period, due_date, status)
VALUES ('inv_2025_08', 'prop_oscar', 'rm_a203', 'tn_dev_1', '2025-08', '2025-08-05', 'open');

INSERT OR IGNORE INTO invoice_items (id, invoice_id, kind, description, amount_satang, sort_order) VALUES
    ('it_2025_08_rent',  'inv_2025_08', 'rent',        'ค่าเช่า', 450000, 1),
    ('it_2025_08_water', 'inv_2025_08', 'water',       'ค่าน้ำ',   28000, 2),
    ('it_2025_08_elec',  'inv_2025_08', 'electricity', 'ค่าไฟ',    39000, 3);

INSERT OR IGNORE INTO payments (id, invoice_id, amount_satang, paid_at, method, reference, idempotency_key)
VALUES ('pay_2025_08', 'inv_2025_08', 517000, '2025-08-03 10:12:00', 'transfer', 'SCB-8842', 'seed-2025-08');
