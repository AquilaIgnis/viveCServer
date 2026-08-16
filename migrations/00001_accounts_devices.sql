-- +goose Up

-- Accounts and devices. The only two tables that exist before any note data does.

CREATE TABLE accounts (
    id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),

    -- Plain `text`, lowercased by the application, rather than `citext`. Case-insensitive
    -- comparison is worth one call at the boundary; it is not worth requiring an extension, which
    -- narrows where this server can be self-hosted (H1) and fails outright on managed Postgres that
    -- will not install one. The unique index below is what actually enforces uniqueness either way.
    email         text        NOT NULL,
    password_hash text        NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),

    -- The account's monotonic change counter, and the whole basis of the pull cursor.
    --
    -- Every push allocates one value from here while holding an advisory lock on the account, so
    -- that sequence order is also commit order. A bare `bigserial` would not give that: a
    -- transaction can take a lower value and commit after a reader has already consumed a higher
    -- one, and the row it wrote is then invisible to that reader for ever. See syncPlan.md SD2.
    change_seq    bigint      NOT NULL DEFAULT 0,

    -- Running total for the per-account quota (S6). Maintained alongside blob writes rather than
    -- summed on demand, because the sum would walk every blob on every upload.
    storage_bytes bigint      NOT NULL DEFAULT 0
);

CREATE UNIQUE INDEX accounts_email_key ON accounts (email);

CREATE TABLE devices (
    id              uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id      uuid        NOT NULL REFERENCES accounts (id) ON DELETE CASCADE,
    name            text        NOT NULL,
    platform        text        NOT NULL,

    -- The SHA-256 of the bearer token, never the token. A database dump must not be enough to
    -- authenticate as somebody: the plaintext is shown once, at registration, and after that only
    -- the device holds it.
    token_hash      bytea       NOT NULL,

    created_at      timestamptz NOT NULL DEFAULT now(),
    last_seen_at    timestamptz,

    -- How far this device has pulled, as last reported. Diagnostic rather than authoritative — the
    -- client sends its own cursor on every pull — but it is what identifies a device whose cursor
    -- has fallen behind the tombstone horizon and needs a full reconcile (SD8).
    last_pulled_seq bigint      NOT NULL DEFAULT 0,

    -- Revocation is a timestamp, not a deleted row, so a revoked device's history stays legible.
    revoked_at      timestamptz
);

CREATE UNIQUE INDEX devices_token_hash_key ON devices (token_hash);
CREATE INDEX devices_account_id_idx ON devices (account_id);

-- +goose Down

DROP TABLE devices;
DROP TABLE accounts;
