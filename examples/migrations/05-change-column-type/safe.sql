-- Expand and contract: add a bigint column, keep it in step with a trigger,
-- copy the old values over in batches, then swap the columns in one short
-- transaction.
SET lock_timeout = '2s';

ALTER TABLE orders ADD COLUMN amount_cents_new bigint;

CREATE FUNCTION orders_amount_cents_sync() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    NEW.amount_cents_new := NEW.amount_cents;
    RETURN NEW;
END
$$;

CREATE TRIGGER orders_amount_cents_sync
    BEFORE INSERT OR UPDATE ON orders
    FOR EACH ROW EXECUTE FUNCTION orders_amount_cents_sync();

-- migrate-safe: batch orders.id 20000
UPDATE orders SET amount_cents_new = amount_cents
WHERE id >= :lo AND id < :hi AND amount_cents_new IS NULL;

ALTER TABLE orders
    ADD CONSTRAINT orders_amount_cents_new_not_null
    CHECK (amount_cents_new IS NOT NULL) NOT VALID;

ALTER TABLE orders VALIDATE CONSTRAINT orders_amount_cents_new_not_null;

BEGIN;
ALTER TABLE orders ALTER COLUMN amount_cents_new SET NOT NULL;
ALTER TABLE orders DROP CONSTRAINT orders_amount_cents_new_not_null;
DROP TRIGGER orders_amount_cents_sync ON orders;
ALTER TABLE orders DROP COLUMN amount_cents;
ALTER TABLE orders RENAME COLUMN amount_cents_new TO amount_cents;
COMMIT;

DROP FUNCTION orders_amount_cents_sync();
