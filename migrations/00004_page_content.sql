-- +goose Up

-- Document bodies (syncPlan.md S3). The fourth synced kind, and the first one the server stores as
-- bytes it cannot read.
--
-- `doc` is opaque by design (SD1): the server never parses `PageDoc`, so a document is a payload
-- with a checksum, a codec id and an encoding id.
--
-- `bytea` rather than `text` because the codec id is a real seam and not a decorative one. The app
-- registers a `cbor/1` codec beside `json/1`, though nothing has ever written with it: its column is
-- TEXT and its repository is typed to a *text* codec, so adopting a binary format there needs a Room
-- migration. Storing bytes here means it would not need a second one on this side as well. Every row
-- that exists today arrives as `json/1`.
--
-- Deliberately its own table rather than columns on `pages`, mirroring the split the app already
-- makes: listing a section must never load every document, and autosave rewrites the body on a
-- 400 ms debounce while the page row it hangs from barely changes.

CREATE TABLE page_content (
    account_id        uuid        NOT NULL REFERENCES accounts (id) ON DELETE CASCADE,
    id                text        NOT NULL,
    version           bigint      NOT NULL CHECK (version > 0),
    change_seq        bigint      NOT NULL CHECK (change_seq > 0),
    deleted_at        bigint,
    updated_at        bigint      NOT NULL,
    server_updated_at timestamptz NOT NULL DEFAULT now(),
    last_writer       uuid        REFERENCES devices (id) ON DELETE SET NULL,
    extra             jsonb       NOT NULL DEFAULT '{}',

    page_id           text        NOT NULL,
    doc               bytea       NOT NULL,

    -- The server computes this on every write and refuses a push whose claimed digest disagrees.
    -- It costs one hash of a payload already in memory and turns a body truncated in transit into a
    -- rejection rather than into a document that decodes to nothing on three devices.
    doc_sha256        bytea       NOT NULL CHECK (octet_length(doc_sha256) = 32),

    -- The codec that wrote `doc`, per row, so a format change is a rolling change rather than a
    -- rewrite of every document (`PageContentEntity.format` in the app).
    format            text        NOT NULL,

    -- How `doc` is wrapped before storage: `none/1` today, the seam an end-to-end encrypted or
    -- compressed body would use (syncPlan.md §3, H4).
    enc               text        NOT NULL DEFAULT 'none/1',

    PRIMARY KEY (account_id, id),

    -- One body per page. The app's own primary key for this table *is* the page id, so this is the
    -- invariant it depends on; enforcing it here rather than by tying `id` to `page_id` with a CHECK
    -- keeps a client that disagrees a rejected entity instead of an aborted batch.
    UNIQUE (account_id, page_id),

    FOREIGN KEY (account_id, page_id) REFERENCES pages (account_id, id) ON DELETE CASCADE
);

-- Every pull is this range scan and nothing else.
CREATE INDEX page_content_account_change_seq_idx ON page_content (account_id, change_seq);

-- +goose Down

DROP TABLE page_content;
