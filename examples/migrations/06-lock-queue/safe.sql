-- With lock_timeout the ALTER gives up after 200 ms instead of waiting
-- behind the report, so the queries queued behind it are released. The
-- deploy then retries later.
SET lock_timeout = '200ms';

-- migrate-safe: long-reader orders 8s
-- migrate-safe: expect-error 55P03
ALTER TABLE orders ADD COLUMN priority smallint NOT NULL DEFAULT 0;
