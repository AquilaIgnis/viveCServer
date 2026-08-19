// Package blobsweep reclaims the disk of attachments nothing references any more (syncPlan.md S5).
//
// The server cannot read a document (§2), so it cannot discover on its own which pictures a page
// still shows. What it has instead is `page_blob_refs`, which the client fills in while pushing
// (SD7), and `attachments`, which is a device saying it still holds a picture. This package turns
// those two into a decision, and takes the long way round on purpose: mark, wait, then delete.
package blobsweep

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AquilaIgnis/viveCServer/internal/blob"
	"github.com/AquilaIgnis/viveCServer/internal/store"
)

// maxDeletionsPerPass bounds one sweep.
//
// A sweep is background work on a server whose foreground job is answering a device that is waiting.
// Deleting in bounded passes keeps a one-off cleanup — an account that just deleted a notebook full
// of photographs — from becoming an hour of unlink calls competing with sync traffic for the same
// disk. What is left over is collected on the next tick, which is the whole reason this runs on one.
const maxDeletionsPerPass = 2000

// Sweeper collects unreferenced attachment bytes on an interval.
type Sweeper struct {
	pool      *pgxpool.Pool
	blobs     blob.Store
	logger    *slog.Logger
	interval  time.Duration
	retention time.Duration
}

func NewSweeper(
	pool *pgxpool.Pool,
	blobs blob.Store,
	logger *slog.Logger,
	interval time.Duration,
	retention time.Duration,
) *Sweeper {
	return &Sweeper{pool: pool, blobs: blobs, logger: logger, interval: interval, retention: retention}
}

// Run sweeps until the context is cancelled.
//
// The first pass waits for one interval rather than running at startup. A server that has just come
// up is a server that may have just been rolled back, restored from a backup, or started against
// the wrong database, and the first thing it does should not be deleting files.
func (sweeper *Sweeper) Run(ctx context.Context) {
	if sweeper.interval <= 0 {
		sweeper.logger.Info("attachment sweeping is disabled")
		return
	}

	ticker := time.NewTicker(sweeper.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			counts, err := sweeper.SweepOnce(ctx)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				// Logged and retried on the next tick rather than fatal. A sweep that cannot run is
				// a server that uses more disk than it needs to, which is not a reason to stop
				// serving anybody's notes.
				sweeper.logger.Error("sweeping attachments failed", "error", err)
				continue
			}
			if counts.RowsDeleted == 0 && counts.Marked == 0 && counts.ReferencesPruned == 0 {
				continue
			}
			sweeper.logger.Info("attachments swept",
				"references_pruned", counts.ReferencesPruned,
				"marked_unreferenced", counts.Marked,
				"referenced_again", counts.Unmarked,
				"rows_deleted", counts.RowsDeleted,
				"files_deleted", counts.FilesDeleted,
				"bytes_freed", counts.BytesFreed,
			)
		}
	}
}

// SweepOnce runs one pass. Exported so a test can drive it without waiting for a tick.
//
// The order of the three phases is the design:
//
//  1. **Prune the references of tombstoned pages.** A page tombstone is kept for the client's
//     benefit (SD8); its picture references are not, and dropping them is what makes "deleting the
//     last page holding a picture frees it" true.
//  2. **Mark what nothing points at, and unmark what something points at again.** Marking is not
//     deleting: the gap between them is the retention window, and it is what protects a client that
//     uploaded bytes it has not yet managed to push a change for.
//  3. **Delete what has been marked for longer than that window**, re-checking the condition inside
//     the transaction that does it, and unlinking the file only when no account anywhere still
//     holds those bytes.
func (sweeper *Sweeper) SweepOnce(ctx context.Context) (store.SweepCounts, error) {
	var counts store.SweepCounts

	// One instance sweeps at a time. Two servers against one database is a supported shape — a
	// rolling restart is exactly that for a few seconds — and both walking the same candidate list
	// would have them contending on every row for no benefit.
	transaction, err := sweeper.pool.Begin(ctx)
	if err != nil {
		return counts, err
	}
	sweeping, err := store.TryLockSweep(ctx, transaction)
	if err != nil {
		_ = transaction.Rollback(ctx)
		return counts, err
	}
	if !sweeping {
		_ = transaction.Rollback(ctx)
		sweeper.logger.Debug("another instance is sweeping attachments")
		return counts, nil
	}

	counts.ReferencesPruned, err = store.PruneReferencesOfDeletedPages(ctx, transaction)
	if err != nil {
		_ = transaction.Rollback(ctx)
		return counts, err
	}

	counts.Marked, counts.Unmarked, err = store.MarkUnreferencedBlobs(ctx, transaction)
	if err != nil {
		_ = transaction.Rollback(ctx)
		return counts, err
	}

	candidates, err := store.SelectSweepableBlobs(ctx, transaction, sweeper.retention, maxDeletionsPerPass)
	if err != nil {
		_ = transaction.Rollback(ctx)
		return counts, err
	}

	// Committed before anything is deleted. The candidate list is advice from here on — every row
	// below is re-checked as it is removed — and holding a transaction open across a few thousand
	// unlink calls would pin one connection and one snapshot for the length of the filesystem work.
	if err := transaction.Commit(ctx); err != nil {
		return counts, err
	}

	for _, candidate := range candidates {
		deleted, bytesFreed, fileDeleted, err := sweeper.deleteOne(ctx, candidate)
		if err != nil {
			if ctx.Err() != nil {
				return counts, nil
			}
			// One blob that will not go is not a reason to abandon the rest of the pass. It is
			// re-examined on the next tick, still marked.
			sweeper.logger.Warn("could not sweep an attachment",
				"account_id", candidate.AccountID, "digest", candidate.Digest, "error", err)
			continue
		}
		if !deleted {
			continue
		}
		counts.RowsDeleted++
		counts.BytesFreed += bytesFreed
		if fileDeleted {
			counts.FilesDeleted++
		}
	}

	return counts, nil
}

// deleteOne removes one account's claim on a digest, and the file behind it when that claim was the
// last one anywhere.
//
// The per-digest lock spans both the database work and the unlink, which is what stops an upload
// arriving in between from ending up with a row whose bytes this pass has already removed. See
// store.BlobLock for the interleaving.
func (sweeper *Sweeper) deleteOne(
	ctx context.Context,
	candidate store.SweepableBlob,
) (deleted bool, bytesFreed int64, fileDeleted bool, err error) {
	lock, err := store.AcquireBlobLock(ctx, sweeper.pool, candidate.Digest)
	if err != nil {
		return false, 0, false, err
	}
	defer lock.Release(ctx)

	deleted, fileUnused, err := store.DeleteSweptBlob(ctx, sweeper.pool, candidate, sweeper.retention)
	if err != nil || !deleted {
		return false, 0, false, err
	}
	if !fileUnused {
		// Another account uploaded the same picture. The bytes stay; only this account's claim on
		// them, and its share of the storage figure, have gone.
		return true, candidate.ByteCount, false, nil
	}

	if err := sweeper.blobs.Remove(candidate.Digest); err != nil {
		// The row is already gone, so this is a leaked file rather than a lost one: nothing points
		// at it, nothing will serve it, and the next upload of the same content reuses it. Reported
		// rather than retried, because retrying a failing unlink on a tick is a log that never ends.
		return true, candidate.ByteCount, false, err
	}
	return true, candidate.ByteCount, true, nil
}
