-- CONCURRENTLY builds the index without blocking writes. It cannot run in a
-- transaction block, and a build that fails leaves an INVALID index behind
-- that has to be dropped before trying again.
CREATE INDEX CONCURRENTLY orders_account_id_idx ON orders (account_id);
