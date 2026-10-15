-- amount_cents is about to overflow integer.
ALTER TABLE orders ALTER COLUMN amount_cents TYPE bigint;
