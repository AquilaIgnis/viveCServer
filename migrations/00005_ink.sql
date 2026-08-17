-- +goose Up

-- Ink (syncPlan.md S4). Three kinds, and the bulk of everything this server will ever store: a
-- notebook may hold 500,000 strokes, against a few thousand rows of everything else put together.
--
-- `ink_strokes` rows are immutable in everything that draws them — points, brush, bounds — with a
-- small mutable envelope: `deleted_at` (an erase tombstones, undo clears it again), `color_argb`
-- with `color_follows_theme` (Switch Background re-resolves an automatic colour), and `group_id`.
-- That is why they take the ordinary OCC path with every other kind rather than SD4's
-- `ON CONFLICT DO NOTHING` fast path: insert-if-absent silently drops both an erase and an undo of
-- one. SD4's *property* still holds — two devices never write the same stroke id, so a conflict
-- here is vanishingly rare and resolves by itself — but a property is not a reason to build a second
-- concurrency mechanism and keep it correct.
--
-- `points` is opaque, like `page_content.doc`: the app's `ink/v1` blob with its encoder id beside
-- it, which this server never decodes. Unlike a document it carries no checksum, because the app
-- stores none to send — `InkStrokeEntity` has no hash column — and a digest computed here from
-- bytes that already arrived would attest to nothing the transport does not. The column can be added
-- later without a client migration for exactly that reason.

CREATE TABLE ink_strokes (
    account_id          uuid        NOT NULL REFERENCES accounts (id) ON DELETE CASCADE,
    id                  text        NOT NULL,
    version             bigint      NOT NULL CHECK (version > 0),
    change_seq          bigint      NOT NULL CHECK (change_seq > 0),
    deleted_at          bigint,
    updated_at          bigint      NOT NULL,
    server_updated_at   timestamptz NOT NULL DEFAULT now(),
    last_writer         uuid        REFERENCES devices (id) ON DELETE SET NULL,
    extra               jsonb       NOT NULL DEFAULT '{}',

    page_id             text        NOT NULL,

    -- Draw order within the page, and a page-scoped Lamport clock rather than a count: the app
    -- allocates MAX(seq) + 1 over every row it holds, pulled rows included, so a stroke drawn on a
    -- device is numbered above everything that device has seen. Two devices drawing offline
    -- therefore allocate the *same* value, which is correct — neither saw the other — and the tie is
    -- broken by id, a UUIDv7 that sorts chronologically. Nothing here may treat it as unique or
    -- dense. viveNotes/memory/inkSyncPlan.md §1.
    seq                 integer     NOT NULL,

    brush_family        text        NOT NULL,
    brush_version       integer     NOT NULL,
    size_dp             real        NOT NULL,
    color_argb          integer     NOT NULL,

    -- Whether color_argb was the automatic colour rather than one the user picked. Null on rows
    -- written before the app recorded the intent, which is a third state and not a default.
    color_follows_theme boolean,

    epsilon             real        NOT NULL,
    stabilization       integer     NOT NULL,

    min_x               real        NOT NULL,
    min_y               real        NOT NULL,
    max_x               real        NOT NULL,
    max_y               real        NOT NULL,

    points              bytea       NOT NULL,
    enc                 text        NOT NULL DEFAULT 'ink/v1',
    created_at          bigint      NOT NULL,
    group_id            text,

    PRIMARY KEY (account_id, id),
    FOREIGN KEY (account_id, page_id) REFERENCES pages (account_id, id) ON DELETE CASCADE
);

-- One eraser gesture: the mask that was drawn, and the ids of the strokes it was allowed to touch.
--
-- `target_ids` travels inside the row rather than as a join table or a kind of its own (SD4). The
-- app's own comment says why the pair must never be split: "the mask without its targets would be
-- unsafe: replaying it against every page stroke would also erase ink drawn later." An array makes
-- that atomicity a property of the row instead of a property of a transaction someone remembers to
-- write.
--
-- The ids are deliberately **not** foreign keys, and one that names no stored stroke is inert. A
-- target recoloured after the operation was made carries a higher change_seq and so reaches a client
-- in a later delta page than the operation naming it; a target the app's seven-day purge has already
-- removed never arrives at all. Under a real reference either case is a row that can never be
-- inserted — and on the client that means a transaction that rolls back with the sync cursor
-- uncommitted, for ever. The app dropped the same foreign keys in its schema 19 for this reason.

CREATE TABLE ink_erases (
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

    -- `Normal` subtracts the mask; `Object` removes every disconnected component it touches. Stored
    -- as text and never checked against a list: a new eraser mode in the app must not need a server
    -- deploy to be storable.
    mode              text        NOT NULL,

    size_dp           real        NOT NULL,
    points            bytea       NOT NULL,
    enc               text        NOT NULL DEFAULT 'ink/v1',
    created_at        bigint      NOT NULL,
    target_ids        text[]      NOT NULL DEFAULT '{}',

    PRIMARY KEY (account_id, id),
    FOREIGN KEY (account_id, page_id) REFERENCES pages (account_id, id) ON DELETE CASCADE
);

-- One lasso transform: the polygon that was drawn, the translation and optional scale composed after
-- it, and the strokes that were inside it. Same target rules as ink_erases.
--
-- A move is stored rather than applied because the strokes it moves are immutable: rewriting their
-- points would invalidate every lasso path already stored against the originals, and would push the
-- whole selection over the wire again on the next sync.

CREATE TABLE ink_moves (
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

    dx_dp             real        NOT NULL,
    dy_dp             real        NOT NULL,
    scale_x           real        NOT NULL DEFAULT 1,
    scale_y           real        NOT NULL DEFAULT 1,
    anchor_x          real        NOT NULL DEFAULT 0,
    anchor_y          real        NOT NULL DEFAULT 0,

    points            bytea       NOT NULL,
    enc               text        NOT NULL DEFAULT 'ink/v1',
    created_at        bigint      NOT NULL,
    target_ids        text[]      NOT NULL DEFAULT '{}',

    PRIMARY KEY (account_id, id),
    FOREIGN KEY (account_id, page_id) REFERENCES pages (account_id, id) ON DELETE CASCADE
);

-- Every pull is these range scans and nothing else.
CREATE INDEX ink_strokes_account_change_seq_idx ON ink_strokes (account_id, change_seq);
CREATE INDEX ink_erases_account_change_seq_idx ON ink_erases (account_id, change_seq);
CREATE INDEX ink_moves_account_change_seq_idx ON ink_moves (account_id, change_seq);

-- +goose Down

DROP TABLE ink_moves;
DROP TABLE ink_erases;
DROP TABLE ink_strokes;
