package store

// Everything one kind's run of a push needs from the database, asked for in one round trip.

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/AquilaIgnis/viveCServer/internal/blob"
)

// ChangeContext is what the four checks guarding a push need to know before any row is written.
type ChangeContext struct {
	// CurrentByID holds the stored rows the run is editing, for the version check.
	CurrentByID map[string]CurrentChange

	// ExistingParentIDs is which of the parents the run names already exist. Existence is the
	// question, not liveness: a tombstoned parent is still a row, the foreign key is still
	// satisfied, and what a deleted subtree means is the client's decision rather than this
	// server's.
	ExistingParentIDs map[string]struct{}

	// StoredBlobs is which of the attachment digests the run references this account holds, and
	// how large each one is.
	StoredBlobs map[string]BlobPresence

	// PurgedIDs is which of the run's own ids this account has erased for good.
	//
	// A device that was offline when the operator pressed Permanently delete still holds the
	// notebook, and if it also holds an unsent edit to it, its next push carries that notebook —
	// with a stale `baseVersion`, which the version check refuses, and then with `baseVersion` 0,
	// which the version check has nothing to say about. That second push would create the notebook
	// again. So the id is retired: nothing may be stored under it again.
	PurgedIDs map[string]struct{}
}

// ReadChangeContext answers all four questions in one round trip.
//
// They have no dependency on one another — each takes the run's ids as an array and reads a
// different table — so sending them one at a time only bought latency. Pipelined, it is one round
// trip per kind.
//
// Unasked questions are skipped rather than sent as queries with empty arrays: a kind with no parent
// has no parent check, and a run naming no attachments has nothing to look up.
func ReadChangeContext(
	ctx context.Context,
	transaction pgx.Tx,
	kind *SyncKind,
	parentKind *SyncKind,
	accountID string,
	entityIDs []string,
	parentIDs []string,
	requiredDigests []string,
) (ChangeContext, error) {
	changeContext := ChangeContext{
		CurrentByID:       make(map[string]CurrentChange, len(entityIDs)),
		ExistingParentIDs: make(map[string]struct{}, len(parentIDs)),
		StoredBlobs:       make(map[string]BlobPresence, len(requiredDigests)),
		PurgedIDs:         make(map[string]struct{}),
	}
	if len(entityIDs) == 0 {
		return changeContext, nil
	}

	pipeline := &pgx.Batch{}
	pipeline.Queue(
		`SELECT id, version, `+kind.changeJSON+` FROM `+kind.Table+
			` WHERE account_id = $1::uuid AND id = ANY($2::text[])`,
		accountID, entityIDs)

	askParents := parentKind != nil && len(parentIDs) > 0
	if askParents {
		pipeline.Queue(
			`SELECT id FROM `+parentKind.Table+` WHERE account_id = $1::uuid AND id = ANY($2::text[])`,
			accountID, parentIDs)
	}

	decodedDigests := decodeDigests(requiredDigests)
	askBlobs := len(decodedDigests) > 0
	if askBlobs {
		pipeline.Queue(`
			SELECT encode(sha256, 'hex'), byte_count
			FROM blobs
			WHERE account_id = $1::uuid AND sha256 = ANY($2::bytea[])`,
			accountID, decodedDigests)
	}

	// Asked for every kind rather than only the one permanent deletion writes today. The log names a
	// kind for exactly this reason, and a check that knew which kinds could appear would be a second
	// copy of that answer.
	pipeline.Queue(
		`SELECT entity_id FROM purges WHERE account_id = $1::uuid AND kind = $2 AND entity_id = ANY($3::text[])`,
		accountID, kind.Name, entityIDs)

	results := transaction.SendBatch(ctx, pipeline)
	defer results.Close()

	if err := eachRow(results, func(rows pgx.Rows) error {
		var entityID string
		var stored CurrentChange
		if err := rows.Scan(&entityID, &stored.Version, &stored.Change); err != nil {
			return err
		}
		changeContext.CurrentByID[entityID] = stored
		return nil
	}); err != nil {
		return ChangeContext{}, fmt.Errorf("reading current %s rows: %w", kind.Name, err)
	}

	if askParents {
		if err := eachRow(results, func(rows pgx.Rows) error {
			var parentID string
			if err := rows.Scan(&parentID); err != nil {
				return err
			}
			changeContext.ExistingParentIDs[parentID] = struct{}{}
			return nil
		}); err != nil {
			return ChangeContext{}, fmt.Errorf("checking %s parents: %w", kind.Name, err)
		}
	}

	if askBlobs {
		if err := eachRow(results, func(rows pgx.Rows) error {
			var digest string
			var presence BlobPresence
			if err := rows.Scan(&digest, &presence.ByteCount); err != nil {
				return err
			}
			changeContext.StoredBlobs[digest] = presence
			return nil
		}); err != nil {
			return ChangeContext{}, fmt.Errorf("reading stored attachments: %w", err)
		}
	}

	if err := eachRow(results, func(rows pgx.Rows) error {
		var entityID string
		if err := rows.Scan(&entityID); err != nil {
			return err
		}
		changeContext.PurgedIDs[entityID] = struct{}{}
		return nil
	}); err != nil {
		return ChangeContext{}, fmt.Errorf("checking purged %s ids: %w", kind.Name, err)
	}

	return changeContext, nil
}

// eachRow reads one result set out of a pipeline, scanning every row through visit.
//
// The rows must be closed before the next result is read, which is what makes this a helper rather
// than four copies of the same loop: forgetting it leaves the pipeline out of step and the next
// result set belongs to the wrong query.
func eachRow(results pgx.BatchResults, visit func(pgx.Rows) error) error {
	rows, err := results.Query()
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		if err := visit(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

// decodeDigests turns wire-form digests into the bytes the column holds, dropping any that cannot be
// one.
//
// Unreachable in practice: callers validate the shape while decoding the push. Skipping rather than
// failing keeps one bad digest a single rejected entity rather than a push that cannot succeed.
func decodeDigests(digests []string) [][]byte {
	decoded := make([][]byte, 0, len(digests))
	for _, digest := range digests {
		raw, err := blob.DecodeDigest(digest)
		if err != nil {
			continue
		}
		decoded = append(decoded, raw)
	}
	return decoded
}
