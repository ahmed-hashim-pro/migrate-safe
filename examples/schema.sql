-- The demo database: an orders table with a few million rows and the
-- accounts it belongs to. `migrate-safe seed` and `migrate-safe examples`
-- replace :rows with --rows.

CREATE TABLE accounts (
    id   bigint PRIMARY KEY,
    name text NOT NULL
);

CREATE TABLE orders (
    id           bigserial PRIMARY KEY,
    account_id   bigint NOT NULL,
    amount_cents integer NOT NULL,
    status       text,
    note         varchar(100),
    created_at   timestamptz NOT NULL DEFAULT now()
);

INSERT INTO accounts (id, name)
SELECT g, 'account ' || g FROM generate_series(1, greatest(:rows / 50, 1000)) g;

INSERT INTO orders (account_id, amount_cents, status, note, created_at)
SELECT 1 + (g::bigint * 7919) % greatest(:rows / 50, 1000),
       (g::bigint * 31 % 100000)::integer,
       (ARRAY['new', 'paid', 'shipped', 'delivered'])[1 + g % 4],
       'order ' || g,
       now() - make_interval(secs => g)
FROM generate_series(1, :rows) g;
