-- Ledger schema. The database, not just the application, enforces the rules
-- that keep money correct:
--
--   1. every transaction balances to zero per currency  (deferred trigger)
--   2. accounts can't go negative unless allowed          (CHECK constraint)
--   3. postings are append-only                           (trigger)
--   4. tenants only ever see their own rows               (row-level security)
--
-- See docs/adr/ for the reasoning behind each one.

CREATE TABLE accounts (
    tenant_id      text        NOT NULL,
    id             text        NOT NULL,
    name           text        NOT NULL CHECK (length(name) BETWEEN 1 AND 200),
    currency       char(3)     NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    allow_negative boolean     NOT NULL DEFAULT false,
    balance        bigint      NOT NULL DEFAULT 0,
    version        bigint      NOT NULL DEFAULT 0,  -- bumps on every posting
    created_at     timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    -- Rule 2. Concurrent transfers can't overdraw an account: the row lock in
    -- PostTransaction serialises them, and this constraint is the backstop.
    CONSTRAINT balance_non_negative CHECK (allow_negative OR balance >= 0)
);

CREATE TABLE transactions (
    tenant_id   text        NOT NULL,
    id          text        NOT NULL,
    description text        NOT NULL DEFAULT '',
    reverses_id text,                       -- set on a reversal
    created_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, reverses_id) REFERENCES transactions (tenant_id, id),
    -- A transaction can be reversed at most once.
    CONSTRAINT reversed_once UNIQUE (tenant_id, reverses_id)
);

CREATE TABLE postings (
    tenant_id      text    NOT NULL,
    transaction_id text    NOT NULL,
    seq            int     NOT NULL,        -- position within the transaction
    account_id     text    NOT NULL,
    currency       char(3) NOT NULL,
    amount         bigint  NOT NULL CHECK (amount <> 0),
    balance_after  bigint  NOT NULL,        -- account balance right after this posting
    account_version bigint NOT NULL,        -- account version right after this posting
    PRIMARY KEY (tenant_id, transaction_id, seq),
    FOREIGN KEY (tenant_id, transaction_id) REFERENCES transactions (tenant_id, id),
    FOREIGN KEY (tenant_id, account_id) REFERENCES accounts (tenant_id, id),
    -- An account's history is a gap-free sequence of versions.
    UNIQUE (tenant_id, account_id, account_version)
);

CREATE INDEX postings_by_account ON postings (tenant_id, account_id, account_version DESC);

-- Idempotency keys: a retried request returns the stored response instead of
-- posting twice. request_hash detects a key reused for a different request.
CREATE TABLE idempotency_keys (
    tenant_id    text        NOT NULL,
    key          text        NOT NULL CHECK (length(key) BETWEEN 1 AND 255),
    method       text        NOT NULL,
    request_hash bytea       NOT NULL,
    response     bytea,                      -- NULL only inside the creating transaction
    created_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, key)
);

-- Rule 1: sum(amount) = 0 per transaction and currency. DEFERRABLE INITIALLY
-- DEFERRED runs the check at COMMIT, after all of a transaction's postings
-- are inserted, so an unbalanced transaction can never commit, whatever the
-- application code does.
CREATE FUNCTION check_transaction_balanced() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    unbalanced record;
BEGIN
    SELECT currency, sum(amount) AS total INTO unbalanced
    FROM postings
    WHERE tenant_id = NEW.tenant_id AND transaction_id = NEW.transaction_id
    GROUP BY currency
    HAVING sum(amount) <> 0
    LIMIT 1;
    IF FOUND THEN
        RAISE EXCEPTION 'transaction % is unbalanced: % % off',
            NEW.transaction_id, unbalanced.total, unbalanced.currency
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NULL;
END $$;

CREATE CONSTRAINT TRIGGER postings_balanced
    AFTER INSERT ON postings
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION check_transaction_balanced();

-- Rule 3: history is immutable. Mistakes are fixed with a reversal.
CREATE FUNCTION reject_mutation() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION '% is append-only; post a reversal instead', TG_TABLE_NAME
        USING ERRCODE = 'insufficient_privilege';
END $$;

CREATE TRIGGER postings_append_only
    BEFORE UPDATE OR DELETE ON postings
    FOR EACH ROW EXECUTE FUNCTION reject_mutation();

CREATE TRIGGER transactions_append_only
    BEFORE UPDATE OR DELETE ON transactions
    FOR EACH ROW EXECUTE FUNCTION reject_mutation();

-- Rule 4: row-level security. The service sets app.tenant_id at the start of
-- every transaction (SET LOCAL). If it forgets, current_setting returns NULL,
-- the comparison is never true, and the query sees nothing: it fails closed.
ALTER TABLE accounts         ENABLE ROW LEVEL SECURITY;
ALTER TABLE transactions     ENABLE ROW LEVEL SECURITY;
ALTER TABLE postings         ENABLE ROW LEVEL SECURITY;
ALTER TABLE idempotency_keys ENABLE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON accounts
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
CREATE POLICY tenant_isolation ON transactions
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
CREATE POLICY tenant_isolation ON postings
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
CREATE POLICY tenant_isolation ON idempotency_keys
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

-- The service connects as ledger_app: not the table owner and without
-- BYPASSRLS, so the policies always apply to it. It can't UPDATE postings or
-- transactions at all, and can't DELETE anything.
CREATE ROLE ledger_app LOGIN PASSWORD 'ledger_app' NOSUPERUSER NOBYPASSRLS;
GRANT SELECT, INSERT          ON transactions, postings TO ledger_app;
GRANT SELECT, INSERT, UPDATE  ON accounts, idempotency_keys TO ledger_app;
