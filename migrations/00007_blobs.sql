-- +goose Up

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

-- +goose Down

-- `accounts.storage_bytes` has existed since 00001 but only this migration's tables ever move it.
-- Dropping them without resetting it would leave every account permanently claiming to hold
-- attachments the rolled-back server has no record of.
UPDATE accounts SET storage_bytes = 0;

DROP TABLE attachments;
DROP TABLE page_blob_refs;
DROP TABLE blobs;
