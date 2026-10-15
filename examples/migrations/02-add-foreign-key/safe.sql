-- NOT VALID enforces the key for new rows straight away without checking
-- the old ones; VALIDATE then checks them under a lock that lets reads and
-- writes continue.
SET lock_timeout = '2s';

ALTER TABLE orders
    ADD CONSTRAINT orders_account_id_fkey
    FOREIGN KEY (account_id) REFERENCES accounts (id) NOT VALID;

ALTER TABLE orders VALIDATE CONSTRAINT orders_account_id_fkey;
