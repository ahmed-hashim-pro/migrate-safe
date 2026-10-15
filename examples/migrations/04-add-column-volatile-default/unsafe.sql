-- Give every order a public identifier.
ALTER TABLE orders ADD COLUMN public_id uuid DEFAULT gen_random_uuid();
