-- Statement types the linter knows, run one after another against the demo
-- schema. The integration test runs this file on a real server and fails if
-- any prediction (lock per table, rewrite, full scan, rows touched) differs
-- from what PostgreSQL did.

CREATE INDEX orders_account_idx ON orders (account_id);
CREATE UNIQUE INDEX orders_id_account_idx ON orders (id, account_id);
DROP INDEX orders_id_account_idx;
CREATE INDEX CONCURRENTLY orders_created_idx ON orders (created_at);
DROP INDEX CONCURRENTLY orders_created_idx;
REINDEX INDEX orders_account_idx;
REINDEX TABLE accounts;

ALTER TABLE orders ALTER COLUMN note TYPE varchar(200);
ALTER TABLE orders ALTER COLUMN note TYPE text;
ALTER TABLE orders ALTER COLUMN note TYPE varchar(150);
ALTER TABLE orders ALTER COLUMN note TYPE varchar;
ALTER TABLE orders ALTER COLUMN amount_cents TYPE bigint;

ALTER TABLE orders ADD COLUMN a1 uuid DEFAULT gen_random_uuid();
ALTER TABLE orders ADD COLUMN a2 integer NOT NULL DEFAULT 0;
ALTER TABLE orders ADD COLUMN a3 timestamptz DEFAULT now();
ALTER TABLE orders ADD COLUMN a4 bigserial;
ALTER TABLE orders ADD COLUMN a5 integer GENERATED ALWAYS AS (amount_cents * 2) STORED;
ALTER TABLE orders ADD COLUMN a6 text;
ALTER TABLE orders ALTER COLUMN a6 SET DEFAULT 'x';
ALTER TABLE orders ALTER COLUMN a6 DROP DEFAULT;
ALTER TABLE orders ADD COLUMN a9 integer, ADD COLUMN a10 text DEFAULT 'y';

ALTER TABLE orders ALTER COLUMN a2 TYPE bigint USING a2::bigint;
ALTER TABLE orders ALTER COLUMN amount_cents SET STATISTICS 500;
ALTER TABLE orders SET (fillfactor = 90);
ALTER TABLE orders RESET (fillfactor);

ALTER TABLE orders ADD CONSTRAINT o_fk FOREIGN KEY (account_id) REFERENCES accounts (id) NOT VALID;
ALTER TABLE orders VALIDATE CONSTRAINT o_fk;
ALTER TABLE orders DROP CONSTRAINT o_fk;
ALTER TABLE orders ADD CONSTRAINT o_fk2 FOREIGN KEY (account_id) REFERENCES accounts (id);

ALTER TABLE orders ADD CONSTRAINT st_nn CHECK (status IS NOT NULL) NOT VALID;
ALTER TABLE orders ALTER COLUMN status SET NOT NULL;
ALTER TABLE orders ALTER COLUMN status DROP NOT NULL;
ALTER TABLE orders VALIDATE CONSTRAINT st_nn;
ALTER TABLE orders ALTER COLUMN status SET NOT NULL;
ALTER TABLE orders ADD CONSTRAINT amount_pos CHECK (amount_cents >= 0);

ALTER TABLE orders ADD CONSTRAINT o_uq UNIQUE (id, account_id);
CREATE UNIQUE INDEX CONCURRENTLY orders_uq2 ON orders (id, created_at);
ALTER TABLE orders ADD CONSTRAINT o_uq2 UNIQUE USING INDEX orders_uq2;

ALTER TABLE orders RENAME COLUMN a6 TO a7;
ALTER TABLE orders DROP COLUMN a7;

CREATE FUNCTION corpus_noop() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RETURN NEW;
END
$$;
CREATE TRIGGER corpus_t BEFORE INSERT ON orders FOR EACH ROW EXECUTE FUNCTION corpus_noop();
ALTER TABLE orders DISABLE TRIGGER corpus_t;
ALTER TABLE orders ENABLE TRIGGER corpus_t;
DROP TRIGGER corpus_t ON orders;

CREATE TABLE refunds (id bigint PRIMARY KEY, order_id bigint REFERENCES orders (id), amount integer);
ALTER TABLE refunds ADD COLUMN reason text;

UPDATE accounts SET name = name || '!';
DELETE FROM orders WHERE id < 10;
-- migrate-safe: batch orders.id 5000
UPDATE orders SET a2 = 1 WHERE id >= :lo AND id < :hi;
INSERT INTO accounts (id, name) VALUES (-1, 'corpus');

BEGIN;
ALTER TABLE orders ADD COLUMN a8 integer;
UPDATE orders SET a8 = 1 WHERE id < 100;
COMMIT;

BEGIN;
LOCK TABLE accounts IN SHARE MODE;
COMMIT;

SET lock_timeout = '5s';
ALTER TABLE orders ADD COLUMN a11 integer;
RESET lock_timeout;

VACUUM orders;
VACUUM FULL accounts;
ANALYZE orders;
CLUSTER accounts USING accounts_pkey;
TRUNCATE refunds;
DROP TABLE refunds;
