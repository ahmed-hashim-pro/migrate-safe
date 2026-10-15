-- On PostgreSQL 12+, SET NOT NULL skips its full-table check when a
-- validated CHECK (status IS NOT NULL) already proves it.
SET lock_timeout = '2s';

ALTER TABLE orders
    ADD CONSTRAINT orders_status_not_null CHECK (status IS NOT NULL) NOT VALID;

ALTER TABLE orders VALIDATE CONSTRAINT orders_status_not_null;

ALTER TABLE orders ALTER COLUMN status SET NOT NULL;

ALTER TABLE orders DROP CONSTRAINT orders_status_not_null;
