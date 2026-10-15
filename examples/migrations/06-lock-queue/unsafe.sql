-- Adding a column with a constant default is metadata-only on PostgreSQL 11+:
-- no rewrite, no scan. It still needs ACCESS EXCLUSIVE for a moment, and here
-- a report has had orders open for a while when the deploy starts.
-- migrate-safe: long-reader orders 8s
ALTER TABLE orders ADD COLUMN priority smallint NOT NULL DEFAULT 0;
