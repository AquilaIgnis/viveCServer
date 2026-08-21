-- +goose Up

-- The whole schema, as one baseline.
--
-- This file is the nine migrations that built the server through S5 folded flat: accounts and
-- devices, the admin session table, the notebook -> section -> page hierarchy, document bodies,
-- ink, attachment blobs, and the purge log. They were squashed before the first release, while the
-- only databases that had ever run them belonged to the developer, so nothing had to be migrated
-- into this shape -- a database either starts here or it does not exist. The two data steps the
-- squashed files carried (resetting `devices.last_pulled_seq`, lifting `closedAt`/`cloudOnlyAt` out
-- of `notebooks.extra`) went with them, because there is no prior data for them to correct.
--
-- **That was a one-time move and it does not come round again.** From here every change is a new
-- file: goose records which versions ran, not what they contained, so editing this one leaves any
-- database that ran the old text silently different from one that runs the new. See migrations/embed.go.

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

    -- Running total of what this account's attachments occupy. Maintained alongside blob writes
    -- rather than summed on demand, because the sum would walk every blob on every upload.
    --
    -- Written for a per-account quota, which was built in S5 and removed the same day: this is a
    -- personal server and the disk is the limit (syncPlan.md §12 decision 1). The column stayed
    -- because the admin dashboard reports it. It is a figure, not a limit — nothing refuses a write
    -- on the strength of it. Only the `blobs` and `attachments` tables below ever move it.
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

    -- The highest cursor this device has later presented back as `since`, which is an
    -- acknowledgement and not a report: a response can be lost after the server writes it and
    -- before the client commits it, so the cursor the server was about to *return* proves nothing.
    -- A cursor the client presents proves it received and committed everything through that
    -- sequence. Permanent deletion depends on that stronger meaning — `purges` rows are pruned once
    -- every live device is past them — and it is also what identifies a device whose cursor has
    -- fallen behind the tombstone horizon and needs a full reconcile (SD8).
    -- See store.RecordAcknowledgedSeq and store.PrunePurges.
    last_pulled_seq bigint      NOT NULL DEFAULT 0,

    -- Revocation is a timestamp, not a deleted row, so a revoked device's history stays legible.
    revoked_at      timestamptz
);

CREATE UNIQUE INDEX devices_token_hash_key ON devices (token_hash);
CREATE INDEX devices_account_id_idx ON devices (account_id);

COMMENT ON COLUMN devices.last_pulled_seq IS
    'Highest pull cursor later presented by the device as since, acknowledging local commit';

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

-- The synced entity kinds, starting with the notebook -> section -> page hierarchy (syncPlan.md S2).
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
    -- gone cannot be described to a device that has not seen it yet. The one exception is the
    -- operator's permanent deletion, which really does remove rows and leaves a `purges` row behind
    -- to say so.
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

    -- The shelf a notebook sits on, as two nullable client wall clocks
    -- (viveNotes/memory/closedNotebooksPlan.md, schema 22).
    --
    -- `closed_at` says the notebook is off the rail. `cloud_only_at` says its bodies, ink and
    -- pictures are held here and not on the device. A cloud-only notebook is always also closed,
    -- but the two answer different questions, which is why they are two columns rather than one
    -- state on the first.
    --
    -- Typed columns rather than the `extra` escape hatch they first landed in. `extra` is what keeps
    -- an older server from dropping a newer client's field; it is not where a field lives once the
    -- server has to act on it. This server does: it refuses to turn a closed notebook's delete into
    -- a tombstone, and the admin panel divides its notebook list by where the bytes are. Both are
    -- predicates, and a predicate over jsonb is one nobody can index or read at a glance.
    closed_at         bigint,
    cloud_only_at     bigint,

    PRIMARY KEY (account_id, id)
);

-- Every pull is this range scan and nothing else.
CREATE INDEX notebooks_account_change_seq_idx ON notebooks (account_id, change_seq);

COMMENT ON COLUMN notebooks.closed_at IS
    'Client wall clock at which the notebook left the rail, or null while it is open';
COMMENT ON COLUMN notebooks.cloud_only_at IS
    'Client wall clock from which this server holds the only copy of the contents, or null';

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

-- Attachment blobs (syncPlan.md S5, SD7, H5).
--
-- Three tables and one rule between them: **the bytes exist before anything points at them, and
-- nothing that points at them can be swept.** A page whose document names a picture the server does
-- not hold is the one failure this phase exists to make impossible, because it is invisible — the
-- document syncs, the page opens on the other device, and the picture is a grey plate for ever.
--
-- The bytes themselves are not here. They live in the content-addressed directory
-- `<VIVE_BLOB_DIR>/<aa>/<bb>/<sha256>` (§8), because a `bytea` column costs the width of every row
-- that walks the table, and a 32 MB photograph does not belong in a table a pull range-scans. This
-- is the same split the app already makes — `filesDir/attachments/<sha256>` beside an `attachments`
-- row — and it is what lets a download stream from a file descriptor to a socket without the
-- server holding the picture in memory at all.

-- Which digests an account holds, and what they cost it.
--
-- Keyed per account although the file on disk is shared between accounts that happen to hold the
-- same bytes. That is deliberate and it is the whole of blob authorisation: **holding the digest is
-- not holding the blob.** A row here is the capability, and an account gets one only by uploading
-- the bytes itself — which it can only do if it already has them. Cross-account dedup therefore
-- costs disk nothing and leaks nothing: knowing a hash never reads somebody else's picture.
CREATE TABLE blobs (
    account_id         uuid        NOT NULL REFERENCES accounts (id) ON DELETE CASCADE,

    -- Raw 32 bytes rather than the 64-character hex the wire uses. Half the index, and the
    -- conversion is one `encode`/`decode` at the edge where the URL is parsed anyway.
    sha256             bytea       NOT NULL CHECK (octet_length(sha256) = 32),

    byte_count         bigint      NOT NULL CHECK (byte_count >= 0),
    created_at         timestamptz NOT NULL DEFAULT now(),

    -- When the sweeper first found nothing pointing at this row, or null while something does.
    --
    -- Two-phase rather than deleting the moment a blob becomes unreachable: a device that pulled a
    -- page a second before another device deleted it is entitled to finish fetching the picture,
    -- and a client that uploads bytes and then loses connectivity before pushing the change that
    -- references them must not have them swept out from under the retry. The mark is cleared again
    -- if anything points at the row before the grace period is up.
    unreferenced_since timestamptz,

    PRIMARY KEY (account_id, sha256)
);

-- "Does any other account still hold these bytes?", which is what decides whether the file on disk
-- may be unlinked when one account's row goes.
CREATE INDEX blobs_sha256_idx ON blobs (sha256);

-- The sweeper's candidate scan. Partial, because in a healthy account almost every row is
-- referenced and a full index would be mostly nulls nobody ever reads.
CREATE INDEX blobs_unreferenced_since_idx ON blobs (unreferenced_since)
    WHERE unreferenced_since IS NOT NULL;

-- Which pages reference which digests — SD7's `blobRefs`, extracted by the client because only the
-- client can read a document.
--
-- The server cannot parse `docJson` (§2), so it cannot discover reachability and cannot garbage
-- collect without being told. The client extracts the attachment ids while pushing; the server
-- stores them here, refuses a push naming a digest it does not hold, and sweeps what nothing names.
CREATE TABLE page_blob_refs (
    account_id uuid  NOT NULL,
    page_id    text  NOT NULL,
    sha256     bytea NOT NULL,

    PRIMARY KEY (account_id, page_id, sha256),

    -- A permanently deleted page takes its references with it, which is how the admin panel's
    -- notebook deletion frees the pictures it was holding without knowing that pictures exist.
    FOREIGN KEY (account_id, page_id) REFERENCES pages (account_id, id) ON DELETE CASCADE,

    -- SD7's invariant, enforced by the database rather than by the care of whoever next edits the
    -- sweeper: a blob that a page still references cannot be deleted. The sweep clears the
    -- references of dead pages first and only then removes the blob, so this fires on a bug and
    -- never in normal operation.
    --
    -- NO ACTION DEFERRABLE rather than RESTRICT because RESTRICT cannot be deferred, and an
    -- immediate check would depend on the order PostgreSQL happens to process two cascades from one
    -- `DELETE FROM accounts`. Deferred to commit, both rows are gone by the time it is checked.
    FOREIGN KEY (account_id, sha256) REFERENCES blobs (account_id, sha256)
        ON DELETE NO ACTION DEFERRABLE INITIALLY DEFERRED
);

-- The reverse direction: "which pages reference this digest", for the reachability sweep and for
-- the foreign key above, which has no index of its own on the referencing side.
CREATE INDEX page_blob_refs_sha256_idx ON page_blob_refs (account_id, sha256);

-- What is known *about* a picture — the eighth synced kind, mirroring AttachmentEntity.
--
-- **Its `id` is the lowercase hex SHA-256 of the bytes**, because that is what the app's own primary
-- key is: `AttachmentEntity.id` is "SHA-256 of the stored bytes, lowercase hex. Also the file's name
-- on disk." So an attachment row and a blob are the same identity seen twice, and a push of one
-- whose bytes are absent is rejected `missing_blob` exactly like a document reference.
--
-- No `page_id`: the app is explicit that one picture can appear on several pages, and a foreign key
-- to a page would have to lie about which one owns it. No `ref_count` either (SD7) — it is a
-- per-device reachability count and each device computes its own.
CREATE TABLE attachments (
    account_id        uuid        NOT NULL REFERENCES accounts (id) ON DELETE CASCADE,
    id                text        NOT NULL,
    version           bigint      NOT NULL CHECK (version > 0),
    change_seq        bigint      NOT NULL CHECK (change_seq > 0),
    deleted_at        bigint,
    updated_at        bigint      NOT NULL,
    server_updated_at timestamptz NOT NULL DEFAULT now(),
    last_writer       uuid        REFERENCES devices (id) ON DELETE SET NULL,
    extra             jsonb       NOT NULL DEFAULT '{}',

    mime_type         text        NOT NULL,

    -- What was stored, after the app's import re-encode to 2048 px on the longest side. Diagnostic
    -- and layout metadata; nothing here is trusted for anything the server enforces.
    pixel_width       integer     NOT NULL,
    pixel_height      integer     NOT NULL,

    -- Checked against the stored blob's real size on every push. A row that disagrees with the
    -- bytes it names is refused, the same way a document body disagreeing with its digest is.
    byte_count        bigint      NOT NULL,

    created_at        bigint      NOT NULL,

    PRIMARY KEY (account_id, id)
);

-- Every pull is this range scan and nothing else (§6).
CREATE INDEX attachments_account_change_seq_idx ON attachments (account_id, change_seq);

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

-- Dropped in reverse dependency order. `accounts` goes last and takes `storage_bytes` with it, so
-- unlike the squashed S5 migration there is no total left behind to reset.

DROP TABLE purges;
DROP TABLE attachments;
DROP TABLE page_blob_refs;
DROP TABLE blobs;
DROP TABLE ink_moves;
DROP TABLE ink_erases;
DROP TABLE ink_strokes;
DROP TABLE page_content;
DROP TABLE applied_batches;
DROP TABLE pages;
DROP TABLE sections;
DROP TABLE notebooks;
DROP TABLE admin_sessions;
DROP TABLE devices;
DROP TABLE accounts;
