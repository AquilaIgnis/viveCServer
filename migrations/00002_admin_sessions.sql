-- +goose Up

-- Browser sessions for the setup/admin surface. Only a SHA-256 digest is stored; the bearer
-- secret lives in the HttpOnly cookie and cannot be recovered from a database backup.
CREATE TABLE admin_sessions (
    token_hash bytea       PRIMARY KEY,
    account_id uuid        NOT NULL REFERENCES accounts (id) ON DELETE CASCADE,
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL
);

CREATE INDEX admin_sessions_account_id_idx ON admin_sessions (account_id);
CREATE INDEX admin_sessions_expires_at_idx ON admin_sessions (expires_at);

-- +goose Down

DROP TABLE admin_sessions;
