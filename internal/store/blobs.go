package store

// The database half of the attachment store (syncPlan.md S5). The bytes are in `internal/blob`;
// what is here is who holds them, what they cost, what still points at them, and the locking that
// keeps those two halves from disagreeing.

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AquilaIgnis/viveCServer/internal/blob"
)

// blobLockNamespace separates per-digest locks from the per-account push lock, which uses
// accountLockNamespace in the same two-int4 key space.
const blobLockNamespace int32 = 0x424c4f42 // "BLOB"

// sweepLockKey is the single key the sweeper serialises on, so two server instances pointed at one
// database do not both walk the same candidates.
const sweepLockKey int32 = 1

// BlobPresence is what the push path needs to know about one stored digest.
type BlobPresence struct {
	ByteCount int64
}

// SelectStoredBlobs reports which of these digests the account holds, and how large each one is.
//
// One query for a whole batch rather than one per entity: a push may carry hundreds of pageContent
// bodies, and a round trip each would make SD7's enforcement the slowest thing about a push.
func SelectStoredBlobs(
	ctx context.Context,
	database Querier,
	accountID string,
	digests []string,
) (map[string]BlobPresence, error) {
	present := make(map[string]BlobPresence, len(digests))
	if len(digests) == 0 {
		return present, nil
	}

	decoded := make([][]byte, 0, len(digests))
	for _, digest := range digests {
		raw, err := blob.DecodeDigest(digest)
		if err != nil {
			// Unreachable: callers validate the shape before asking. Skipping rather than failing
			// keeps a malformed digest a rejected entity instead of a failed push.
			continue
		}
		decoded = append(decoded, raw)
	}

	const statement = `
		SELECT encode(sha256, 'hex'), byte_count
		FROM blobs
		WHERE account_id = $1::uuid AND sha256 = ANY($2::bytea[])`

	rows, err := database.Query(ctx, statement, accountID, decoded)
	if err != nil {
		return nil, fmt.Errorf("checking stored blobs: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var digest string
		var presence BlobPresence
		if err := rows.Scan(&digest, &presence.ByteCount); err != nil {
			return nil, fmt.Errorf("checking stored blobs: %w", err)
		}
		present[digest] = presence
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("checking stored blobs: %w", err)
	}
	return present, nil
}

// RecordStoredBlob claims bytes for an account and reports whether this call is what created the row.
//
// A row that already exists is left alone and reported as `stored=false`. That is the dedup answer
// on the database side: the same picture arriving from a second device costs one index probe.
//
// There is no quota to check. This is a personal server (syncPlan.md §12 decision 1) — the operator
// and the user are the same person, the disk is the limit, and a quota would only turn "full disk"
// into "refused earlier". `accounts.storage_bytes` is still maintained, because the admin dashboard
// reports it.
func RecordStoredBlob(
	ctx context.Context,
	pool *pgxpool.Pool,
	accountID string,
	digest string,
	byteCount int64,
) (stored bool, err error) {
	rawDigest, err := blob.DecodeDigest(digest)
	if err != nil {
		return false, err
	}

	transaction, err := pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("starting a blob write: %w", err)
	}
	defer func() { _ = transaction.Rollback(ctx) }()

	const insertStatement = `
		INSERT INTO blobs (account_id, sha256, byte_count)
		VALUES ($1::uuid, $2, $3)
		ON CONFLICT (account_id, sha256) DO NOTHING`

	commandTag, err := transaction.Exec(ctx, insertStatement, accountID, rawDigest, byteCount)
	if err != nil {
		return false, fmt.Errorf("recording a stored blob: %w", err)
	}
	if commandTag.RowsAffected() == 0 {
		// Already held. Nothing to charge, and the caller keeps the bytes it already published.
		if err := transaction.Commit(ctx); err != nil {
			return false, fmt.Errorf("committing a blob write: %w", err)
		}
		return false, nil
	}

	const chargeStatement = `
		UPDATE accounts
		SET storage_bytes = storage_bytes + $2
		WHERE id = $1::uuid`

	if _, err := transaction.Exec(ctx, chargeStatement, accountID, byteCount); err != nil {
		return false, fmt.Errorf("charging stored bytes: %w", err)
	}

	if err := transaction.Commit(ctx); err != nil {
		return false, fmt.Errorf("committing a blob write: %w", err)
	}
	return true, nil
}

// AccountStorageBytes is what an account's attachments occupy. It is what the admin dashboard's
// storage tile reports, and it is not a limit: nothing refuses a write on the strength of it.
func AccountStorageBytes(ctx context.Context, database Querier, accountID string) (int64, error) {
	var storageBytes int64
	err := database.QueryRow(ctx, `SELECT storage_bytes FROM accounts WHERE id = $1::uuid`, accountID).Scan(&storageBytes)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrAccountNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("reading stored bytes: %w", err)
	}
	return storageBytes, nil
}

// AnyAccountHoldsBlob reports whether any account still claims these bytes.
//
// The file on disk is shared by every account that uploaded the same picture, so "may I unlink it"
// is a question about the whole table rather than about one account's row. Callers ask it while
// holding the digest's [BlobLock], which is what keeps the answer true for as long as they act on
// it.
func AnyAccountHoldsBlob(ctx context.Context, database Querier, digest string) (bool, error) {
	rawDigest, err := blob.DecodeDigest(digest)
	if err != nil {
		return false, err
	}

	var held bool
	if err := database.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM blobs WHERE sha256 = $1)`, rawDigest,
	).Scan(&held); err != nil {
		return false, fmt.Errorf("checking blob holders: %w", err)
	}
	return held, nil
}

// BlobLock is a session-scoped advisory lock on one digest.
//
// It exists to close exactly one race, which is otherwise unclosable and silently destructive:
//
//	uploader: publishes the file           sweeper: decides this digest is unreferenced
//	uploader: inserts the row              sweeper: deletes the row, commits
//	                                       sweeper: unlinks the file
//	→ the uploader's row now claims bytes that are gone.
//
// Both sides take this lock across *both* of their steps — the database write and the filesystem
// operation — which is why it cannot be a transaction-scoped lock: `pg_advisory_xact_lock` is
// released at commit, and the unlink happens after the commit on purpose (bytes outlive their row
// safely; a row outliving its bytes is the corruption).
//
// It is held on its own pooled connection for the length of a rename or an unlink, never for the
// length of an upload.
type BlobLock struct {
	connection *pgxpool.Conn
	key        int32
}

// AcquireBlobLock takes the per-digest lock. The caller must Release it.
func AcquireBlobLock(ctx context.Context, pool *pgxpool.Pool, digest string) (*BlobLock, error) {
	connection, err := pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquiring a connection for a blob lock: %w", err)
	}

	key := blobLockKey(digest)
	if _, err := connection.Exec(ctx, `SELECT pg_advisory_lock($1, $2)`, blobLockNamespace, key); err != nil {
		connection.Release()
		return nil, fmt.Errorf("locking a blob: %w", err)
	}
	return &BlobLock{connection: connection, key: key}, nil
}

// Release drops the lock and returns the connection to the pool.
//
// A session lock outlives its transaction, so a connection handed back still holding one would
// deadlock whichever request borrows it next. If the unlock itself fails the connection is
// destroyed instead of reused: ending the session is the one remaining way to release the lock.
func (lock *BlobLock) Release(ctx context.Context) {
	if _, err := lock.connection.Exec(ctx, `SELECT pg_advisory_unlock($1, $2)`, blobLockNamespace, lock.key); err != nil {
		lock.connection.Conn().Close(ctx)
	}
	lock.connection.Release()
}

func blobLockKey(digest string) int32 {
	hash := fnv.New32a()
	_, _ = hash.Write([]byte(digest))
	return int32(hash.Sum32())
}

// SweepCounts reports what one garbage-collection pass did.
type SweepCounts struct {
	ReferencesPruned int64
	Marked           int64
	Unmarked         int64
	RowsDeleted      int64
	FilesDeleted     int64
	BytesFreed       int64
}

// SweepableBlob is one row the sweeper has removed and may now have to unlink.
type SweepableBlob struct {
	AccountID string
	Digest    string
	ByteCount int64
}

// PruneReferencesOfDeletedPages drops `page_blob_refs` rows belonging to tombstoned pages.
//
// This is what makes "deleting its last page frees the picture" true (§9's S5 exit criterion). It
// runs before the mark below, and it is also why the foreign key from page_blob_refs to blobs can
// be a hard constraint: by the time a blob is considered for deletion, nothing that is still alive
// points at it.
//
// A page tombstone is retained for the client's benefit, not the blob's. Its references are not:
// once the delete is durable, no live document names those bytes, and a device that has not yet
// pulled the delete is about to stop wanting them.
func PruneReferencesOfDeletedPages(ctx context.Context, database Querier) (int64, error) {
	const statement = `
		DELETE FROM page_blob_refs AS reference
		USING pages
		WHERE pages.account_id = reference.account_id
		  AND pages.id = reference.page_id
		  AND pages.deleted_at IS NOT NULL`

	commandTag, err := database.Exec(ctx, statement)
	if err != nil {
		return 0, fmt.Errorf("pruning references of deleted pages: %w", err)
	}
	return commandTag.RowsAffected(), nil
}

// reachabilityCondition is the definition of "something still wants these bytes", written once
// because the mark and the unmark must not be able to disagree about it.
//
// Two roots, and the union of them is what keeps the guarantee honest:
//
//   - a live page whose document references the digest (SD7), and
//   - a live `attachments` row for it, which is a device saying it still holds the picture.
//
// Either alone is insufficient. Pages alone would sweep the bytes behind an attachment a device
// still has in its own store, and the device would then be unable to re-upload from a page it no
// longer has. Attachments alone would leak every picture whose metadata row a client never
// tombstoned — and no client has to, since `refCount` is per-device and never synced (SD7).
//
// The attachment join compares hex to hex rather than decoding the id. `decode()` on an id that is
// not hex raises, and one bad row would abort every sweep for ever; `encode()` cannot fail, and the
// comparison still seeks the (account_id, id) primary key.
const reachabilityCondition = `(
		EXISTS (
			SELECT 1
			FROM page_blob_refs AS reference
			JOIN pages ON pages.account_id = reference.account_id AND pages.id = reference.page_id
			WHERE reference.account_id = blobs.account_id
			  AND reference.sha256 = blobs.sha256
			  AND pages.deleted_at IS NULL
		)
		OR EXISTS (
			SELECT 1
			FROM attachments
			WHERE attachments.account_id = blobs.account_id
			  AND attachments.id = encode(blobs.sha256, 'hex')
			  AND attachments.deleted_at IS NULL
		)
	)`

// MarkUnreferencedBlobs timestamps blobs nothing points at, and clears the mark on any that
// something points at again.
//
// Marking is not deleting. The gap between them is the grace period, and it is what protects the
// two cases a single-pass sweep gets wrong: bytes uploaded a moment ago by a client whose push has
// not arrived yet, and bytes a second device is still downloading for a page that has just been
// deleted underneath it.
func MarkUnreferencedBlobs(ctx context.Context, database Querier) (marked int64, unmarked int64, err error) {
	const markStatement = `
		UPDATE blobs SET unreferenced_since = now()
		WHERE unreferenced_since IS NULL AND NOT ` + reachabilityCondition

	markTag, err := database.Exec(ctx, markStatement)
	if err != nil {
		return 0, 0, fmt.Errorf("marking unreferenced blobs: %w", err)
	}

	const unmarkStatement = `
		UPDATE blobs SET unreferenced_since = NULL
		WHERE unreferenced_since IS NOT NULL AND ` + reachabilityCondition

	unmarkTag, err := database.Exec(ctx, unmarkStatement)
	if err != nil {
		return 0, 0, fmt.Errorf("clearing blob marks: %w", err)
	}
	return markTag.RowsAffected(), unmarkTag.RowsAffected(), nil
}

// SelectSweepableBlobs lists rows that have been unreferenced for longer than the grace period.
func SelectSweepableBlobs(
	ctx context.Context,
	database Querier,
	grace time.Duration,
	limit int,
) ([]SweepableBlob, error) {
	const statement = `
		SELECT account_id::text, encode(sha256, 'hex'), byte_count
		FROM blobs
		WHERE unreferenced_since IS NOT NULL AND unreferenced_since < now() - $1::interval
		ORDER BY unreferenced_since
		LIMIT $2`

	rows, err := database.Query(ctx, statement, grace, limit)
	if err != nil {
		return nil, fmt.Errorf("listing sweepable blobs: %w", err)
	}
	defer rows.Close()

	sweepable := make([]SweepableBlob, 0, limit)
	for rows.Next() {
		var candidate SweepableBlob
		if err := rows.Scan(&candidate.AccountID, &candidate.Digest, &candidate.ByteCount); err != nil {
			return nil, fmt.Errorf("listing sweepable blobs: %w", err)
		}
		sweepable = append(sweepable, candidate)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing sweepable blobs: %w", err)
	}
	return sweepable, nil
}

// DeleteSweptBlob removes one account's claim on a digest and reports whether any account still
// holds it.
//
// The re-check inside the transaction is not the same question as the one the candidate list
// answered: the row may have become referenced again since, and the `WHERE` below is what makes
// the decision atomic with the deletion rather than merely recent.
//
// Returns `fileUnused` only when no row anywhere in the database still names those bytes, because
// the file on disk is shared by every account that uploaded the same picture.
func DeleteSweptBlob(
	ctx context.Context,
	pool *pgxpool.Pool,
	candidate SweepableBlob,
	grace time.Duration,
) (deleted bool, fileUnused bool, err error) {
	rawDigest, err := blob.DecodeDigest(candidate.Digest)
	if err != nil {
		return false, false, err
	}

	transaction, err := pool.Begin(ctx)
	if err != nil {
		return false, false, fmt.Errorf("starting a blob sweep: %w", err)
	}
	defer func() { _ = transaction.Rollback(ctx) }()

	const deleteStatement = `
		DELETE FROM blobs
		WHERE account_id = $1::uuid
		  AND sha256 = $2
		  AND unreferenced_since IS NOT NULL
		  AND unreferenced_since < now() - $3::interval
		RETURNING byte_count`

	var byteCount int64
	err = transaction.QueryRow(ctx, deleteStatement, candidate.AccountID, rawDigest, grace).Scan(&byteCount)
	if errors.Is(err, pgx.ErrNoRows) {
		// Referenced again, or already swept by another instance. Both are ordinary.
		return false, false, nil
	}
	if err != nil {
		return false, false, fmt.Errorf("deleting a swept blob: %w", err)
	}

	const refundStatement = `
		UPDATE accounts
		SET storage_bytes = GREATEST(storage_bytes - $2, 0)
		WHERE id = $1::uuid`

	if _, err := transaction.Exec(ctx, refundStatement, candidate.AccountID, byteCount); err != nil {
		return false, false, fmt.Errorf("refunding swept bytes: %w", err)
	}

	const remainingStatement = `SELECT EXISTS (SELECT 1 FROM blobs WHERE sha256 = $1)`

	var stillHeld bool
	if err := transaction.QueryRow(ctx, remainingStatement, rawDigest).Scan(&stillHeld); err != nil {
		return false, false, fmt.Errorf("checking remaining blob holders: %w", err)
	}

	if err := transaction.Commit(ctx); err != nil {
		return false, false, fmt.Errorf("committing a blob sweep: %w", err)
	}
	return true, !stillHeld, nil
}

// TryLockSweep takes the exclusive lock one sweep runs under, so that two instances pointed at one
// database do not walk the same candidates. A sweep that cannot take it simply skips this tick.
func TryLockSweep(ctx context.Context, transaction pgx.Tx) (bool, error) {
	var acquired bool
	err := transaction.QueryRow(ctx,
		`SELECT pg_try_advisory_xact_lock($1, $2)`, blobLockNamespace, sweepLockKey,
	).Scan(&acquired)
	if err != nil {
		return false, fmt.Errorf("locking the blob sweep: %w", err)
	}
	return acquired, nil
}
