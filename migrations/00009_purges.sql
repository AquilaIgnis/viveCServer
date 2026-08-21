-- +goose Up

-- The account's record of what it has erased for good, and the queue that tells the devices.
--
-- Every other delete in this schema is a tombstone, because a row that is simply gone cannot be
-- described to a device that has not seen it yet. Permanent deletion is the one operation that
-- really does remove rows -- the operator asked for the disk back -- so it needs somewhere to leave
-- the sentence "this id is gone" once the row that carried it no longer exists.
--
-- That makes this table two things at once, and it has to be both for either to be safe:
--
--   * the queue. A purge takes a change sequence value of its own, so the account cursor moves and
--     every device notices on its next idle poll. The pull returns it in `purges`, beside the
--     changes and under the same cursor, and a device that still holds the notebook drops it.
--   * the gravestone. A push naming a purged id is refused rather than stored. Without that, a
--     device that was offline with an unsent edit would push the notebook back as new the moment it
--     reconnected, and the account would get its erased notebook returned to it by the one device
--     that had not heard.
--
-- Both duties end at the same moment -- when every active device has pulled past `change_seq` --
-- which is when a row here is pruned. That is the same condition that used to *block* the
-- operator's Permanently delete button; it now governs a hundred bytes of housekeeping instead of
-- the operator, which is the whole point of the change.
CREATE TABLE purges (
    account_id uuid        NOT NULL REFERENCES accounts (id) ON DELETE CASCADE,

    -- The entity kind, as the sync protocol names it. Only `notebook` is ever written here today:
    -- permanent deletion happens at the notebook root and PostgreSQL cascades the subtree away, so
    -- naming the root is enough for the client, whose Room schema cascades the same way. The column
    -- exists so that a second purgeable kind is a row rather than a table.
    kind       text        NOT NULL,

    -- The client-generated id the erased entity had. This is the only trace the account keeps of it,
    -- and it keeps none of its content -- no name, no bodies, no ink.
    entity_id  text        NOT NULL,

    -- The account-wide sequence value this purge was allocated under, exactly like a change. It is
    -- what puts the purge in the pull window `(since, cursor]` and what a device's acknowledged
    -- cursor is compared against before the row is pruned.
    change_seq bigint      NOT NULL CHECK (change_seq > 0),

    -- Server truth, like `server_updated_at` elsewhere: client wall clocks are display metadata and
    -- may be wrong (syncPlan.md SD1), and nothing here was written by a client anyway.
    purged_at  timestamptz NOT NULL DEFAULT now(),

    -- An id is erased once. A second permanent deletion of the same id is impossible while the row
    -- is gone, and the key says so rather than trusting that.
    PRIMARY KEY (account_id, kind, entity_id)
);

-- The pull window, which is the same range scan a delta does over each kind's table.
CREATE INDEX purges_account_change_seq_idx ON purges (account_id, change_seq);

-- +goose Down

DROP TABLE purges;
