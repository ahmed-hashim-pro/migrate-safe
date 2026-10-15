-- Application traffic replayed while a migration runs. Each class gets its
-- own workers; :id and :account are drawn at random for every query from the
-- ranges of the columns named below.

-- param: id = orders.id
-- param: account = accounts.id
-- think: 2ms

-- class: read workers=4
SELECT id, status, amount_cents FROM orders WHERE id = :id;

-- class: write workers=4
UPDATE orders SET status = 'shipped' WHERE id = :id;

-- class: insert workers=2
INSERT INTO orders (account_id, amount_cents, status, note)
VALUES (:account, 1999, 'new', 'placed during the migration');
