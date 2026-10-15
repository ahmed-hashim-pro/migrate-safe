-- Every order must belong to an account.
ALTER TABLE orders
    ADD CONSTRAINT orders_account_id_fkey
    FOREIGN KEY (account_id) REFERENCES accounts (id);
