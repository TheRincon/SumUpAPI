CREATE EXTENSION fuzzystrmatch;

CREATE TABLE accounts (
    id            TEXT   PRIMARY KEY,
    name          TEXT   NOT NULL,
    address       TEXT   NOT NULL,
    date_of_birth DATE   NOT NULL,
    id_number     TEXT   NOT NULL UNIQUE,
    balance       BIGINT NOT NULL DEFAULT 0 CHECK (balance >= 0)
);

CREATE INDEX accounts_date_of_birth_idx ON accounts (date_of_birth);

-- One row per change to one account's balance. A transfer writes two rows that
-- share a transaction_id and net to zero.
CREATE TABLE ledger_entries (
    id                      BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    transaction_id          TEXT        NOT NULL,
    account_id              TEXT        NOT NULL REFERENCES accounts (id),
    type                    TEXT        NOT NULL CHECK (type IN ('opening_balance', 'deposit', 'transfer')),
    amount                  BIGINT      NOT NULL CHECK (amount <> 0),
    balance_after           BIGINT      NOT NULL CHECK (balance_after >= 0),
    counterparty_account_id TEXT        REFERENCES accounts (id),
    created_at              TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    CHECK ((type = 'transfer') = (counterparty_account_id IS NOT NULL))
);

CREATE INDEX ledger_entries_account_idx ON ledger_entries (account_id, id DESC);

-- Inserting a ledger entry is the only way to change a balance. The update runs
-- inside the INSERT statement, so the two succeed or fail as one.
CREATE FUNCTION apply_ledger_entry() RETURNS trigger AS $$
BEGIN
    UPDATE accounts SET balance = balance + NEW.amount
    WHERE id = NEW.account_id
    RETURNING balance INTO NEW.balance_after;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER ledger_entries_apply
    BEFORE INSERT ON ledger_entries
    FOR EACH ROW EXECUTE FUNCTION apply_ledger_entry();

-- Rejects any balance write that does not come from apply_ledger_entry, where
-- this trigger runs nested at depth 2.
CREATE FUNCTION reject_direct_balance_change() RETURNS trigger AS $$
BEGIN
    IF pg_trigger_depth() = 1 AND (TG_OP = 'UPDATE' OR NEW.balance <> 0) THEN
        RAISE EXCEPTION 'balances change only through ledger_entries';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER accounts_balance_guard
    BEFORE INSERT OR UPDATE OF balance ON accounts
    FOR EACH ROW EXECUTE FUNCTION reject_direct_balance_change();

CREATE FUNCTION reject_ledger_change() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'ledger_entries is append-only';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER ledger_entries_append_only
    BEFORE UPDATE OR DELETE ON ledger_entries
    FOR EACH ROW EXECUTE FUNCTION reject_ledger_change();

CREATE TRIGGER ledger_entries_no_truncate
    BEFORE TRUNCATE ON ledger_entries
    FOR EACH STATEMENT EXECUTE FUNCTION reject_ledger_change();
