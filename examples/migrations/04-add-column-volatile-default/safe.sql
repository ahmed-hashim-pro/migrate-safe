-- A volatile default needs a different value in every existing row, so
-- PostgreSQL rewrites the table to fill it. Add the column empty, set the
-- default for new rows, and fill the old rows in small batches instead.
SET lock_timeout = '2s';

ALTER TABLE orders ADD COLUMN public_id uuid;

ALTER TABLE orders ALTER COLUMN public_id SET DEFAULT gen_random_uuid();

-- migrate-safe: batch orders.id 20000
UPDATE orders SET public_id = gen_random_uuid()
WHERE id >= :lo AND id < :hi AND public_id IS NULL;
