-- Wallet Transfer Service schema. Idempotent (safe to run on every startup).
-- All amounts are BIGINT in minor units (e.g. paise/cents) -> no floating point.

CREATE TABLE IF NOT EXISTS wallets (
    id          TEXT        PRIMARY KEY,
    balance     BIGINT      NOT NULL DEFAULT 0 CHECK (balance >= 0),   -- last line of defence against overdraft
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS transfers (
    id              TEXT        PRIMARY KEY,
    idempotency_key TEXT        UNIQUE,                                 -- NULL when client sends no key
    from_wallet_id  TEXT        NOT NULL REFERENCES wallets(id),
    to_wallet_id    TEXT        NOT NULL REFERENCES wallets(id),
    amount          BIGINT      NOT NULL CHECK (amount > 0),
    status          TEXT        NOT NULL CHECK (status IN ('PENDING', 'PROCESSED', 'FAILED')),
    failure_reason  TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (from_wallet_id <> to_wallet_id)
);
CREATE INDEX IF NOT EXISTS transfers_from_wallet_idx ON transfers (from_wallet_id, created_at DESC);
CREATE INDEX IF NOT EXISTS transfers_to_wallet_idx   ON transfers (to_wallet_id, created_at DESC);

CREATE TABLE IF NOT EXISTS ledger_entries (
    entry_id    BIGSERIAL   PRIMARY KEY,
    wallet_id   TEXT        NOT NULL REFERENCES wallets(id),
    transfer_id TEXT        NOT NULL REFERENCES transfers(id),
    type        TEXT        NOT NULL CHECK (type IN ('DEBIT', 'CREDIT')),
    amount      BIGINT      NOT NULL CHECK (amount > 0),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (transfer_id, type)                                          -- at most one DEBIT + one CREDIT per transfer
);
CREATE INDEX IF NOT EXISTS ledger_entries_wallet_idx ON ledger_entries (wallet_id, entry_id);

CREATE TABLE IF NOT EXISTS idempotency_records (
    key          TEXT        PRIMARY KEY,
    request_hash TEXT        NOT NULL,                                  -- detects same key + different payload
    transfer_id  TEXT        NOT NULL REFERENCES transfers(id) DEFERRABLE INITIALLY DEFERRED,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
