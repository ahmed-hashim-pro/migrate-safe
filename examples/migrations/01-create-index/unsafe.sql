-- Index orders by account, for the account page.
CREATE INDEX orders_account_id_idx ON orders (account_id);
