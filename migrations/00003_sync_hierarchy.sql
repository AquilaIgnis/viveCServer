-- +goose Up

-- The first synced entity kinds: the notebook -> section -> page hierarchy (syncPlan.md S2).
--
-- Every synced table repeats one envelope -- account_id, id, version, change_seq, deleted_at,
-- updated_at, server_updated_at, last_writer, extra -- and adds its own columns after it. The shape
-- is copied per table rather than inherited: PostgreSQL table inheritance carries neither indexes
-- nor foreign keys down to children, and a shared parent table would make every pull touch every
-- kind. The repetition is what keeps one pull one index range scan per kind.

CREATE TABLE notebooks (
    account_id        uuid        NOT NULL REFERENCES accounts (id) ON DELETE CASCADE,

    -- Client-generated, not server-assigned. The app creates entities while offline and must never
    -- wait for a round trip to name one. `text` rather than `uuid` because the ids are opaque
    -- strings to this server, which is the same reason it never parses a document.
    id                text        NOT NULL,

    -- Server-assigned, incremented on every accepted write. This is the whole of optimistic
    -- concurrency control (SD1): a push carries the version it believes it is editing and applies
    -- only while that is still the stored value.
    version           bigint      NOT NULL CHECK (version > 0),

    -- The account-wide sequence value this row was last written under, allocated once per push
    -- while holding the account's advisory lock (SD2). Pull is `change_seq > cursor`, so this
    -- column is the only thing that makes a delta possible.
    change_seq        bigint      NOT NULL CHECK (change_seq > 0),

    -- Tombstone. A delete is an ordinary write that sets this, never a SQL DELETE: a row that is
    -- gone cannot be described to a device that has not seen it yet.
    deleted_at        bigint,

    -- The writing client's wall clock, kept for display and for the client's merge heuristics.
    -- Nothing about convergence depends on it (SD1, R3), which is why the server's own timestamp is
    -- stored beside it rather than instead of it.
    updated_at        bigint      NOT NULL,
    server_updated_at timestamptz NOT NULL DEFAULT now(),

    -- Diagnostic: which device wrote this row last. Nulled rather than blocking if a device row
    -- ever goes away, because losing the attribution must not lose the note.
    last_writer       uuid        REFERENCES devices (id) ON DELETE SET NULL,

    -- Fields this server does not recognise, stored verbatim and returned verbatim (SD5). A newer
    -- client can ship a field before the server knows about it and an older server will not drop
    -- it, which is what stops app and server releases from having to move in lockstep.
    extra             jsonb       NOT NULL DEFAULT '{}',

    name              text        NOT NULL,
    color_argb        integer     NOT NULL,
    sort_index        integer     NOT NULL,
    expanded          boolean     NOT NULL,
    created_at        bigint      NOT NULL,

    PRIMARY KEY (account_id, id)
);

-- Every pull is this range scan and nothing else.
CREATE INDEX notebooks_account_change_seq_idx ON notebooks (account_id, change_seq);

CREATE TABLE sections (
    account_id        uuid        NOT NULL REFERENCES accounts (id) ON DELETE CASCADE,
    id                text        NOT NULL,
    version           bigint      NOT NULL CHECK (version > 0),
    change_seq        bigint      NOT NULL CHECK (change_seq > 0),
    deleted_at        bigint,
    updated_at        bigint      NOT NULL,
    server_updated_at timestamptz NOT NULL DEFAULT now(),
    last_writer       uuid        REFERENCES devices (id) ON DELETE SET NULL,
    extra             jsonb       NOT NULL DEFAULT '{}',

    notebook_id       text        NOT NULL,
    name              text        NOT NULL,
    color_argb        integer     NOT NULL,
    sort_index        integer     NOT NULL,
    created_at        bigint      NOT NULL,

    PRIMARY KEY (account_id, id),

    -- A real foreign key, not just an application check (SD5). The push handler rejects an unknown
    -- parent with `missing_parent` before it gets here, so this constraint should never fire; it
    -- exists so that a bug in that check cannot leave the server holding a section whose notebook
    -- was never uploaded.
    FOREIGN KEY (account_id, notebook_id) REFERENCES notebooks (account_id, id) ON DELETE CASCADE
);

CREATE INDEX sections_account_change_seq_idx ON sections (account_id, change_seq);
CREATE INDEX sections_account_notebook_idx ON sections (account_id, notebook_id);

CREATE TABLE pages (
    account_id        uuid        NOT NULL REFERENCES accounts (id) ON DELETE CASCADE,
    id                text        NOT NULL,
    version           bigint      NOT NULL CHECK (version > 0),
    change_seq        bigint      NOT NULL CHECK (change_seq > 0),
    deleted_at        bigint,
    updated_at        bigint      NOT NULL,
    server_updated_at timestamptz NOT NULL DEFAULT now(),
    last_writer       uuid        REFERENCES devices (id) ON DELETE SET NULL,
    extra             jsonb       NOT NULL DEFAULT '{}',

    section_id        text        NOT NULL,
    title             text        NOT NULL,
    sort_index        integer     NOT NULL,

    -- Denormalised first line of body text. It is a field of the page row in the app rather than
    -- something derived from the document, so it syncs with the page and the server still never
    -- reads a document to produce it.
    preview           text        NOT NULL,
    created_at        bigint      NOT NULL,

    PRIMARY KEY (account_id, id),
    FOREIGN KEY (account_id, section_id) REFERENCES sections (account_id, id) ON DELETE CASCADE
);

CREATE INDEX pages_account_change_seq_idx ON pages (account_id, change_seq);
CREATE INDEX pages_account_section_idx ON pages (account_id, section_id);

-- The idempotency cache for pushes.
--
-- A push whose response is lost is ordinary rather than exceptional on a mobile network, and the
-- client's only recovery is to send the same batch again. Without this table that retry re-applies
-- the work: every entity rejects against the client's own previous write, and the account burns a
-- second sequence number describing nothing. The stored response is replayed byte for byte instead.
CREATE TABLE applied_batches (
    device_id  uuid        NOT NULL REFERENCES devices (id) ON DELETE CASCADE,

    -- Client-generated idempotency key, one per attempt at a batch and reused across its retries.
    batch_id   uuid        NOT NULL,

    response   jsonb       NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (device_id, batch_id)
);

CREATE INDEX applied_batches_created_at_idx ON applied_batches (created_at);

-- +goose Down

DROP TABLE applied_batches;
DROP TABLE pages;
DROP TABLE sections;
DROP TABLE notebooks;
