package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// NotebookSummary is one notebook as the admin panel shows it.
//
// Deliberately not NotebookFields: the dashboard wants a name and enough context to tell two
// notebooks called "Work" apart, not the row the sync protocol pushes around.
type NotebookSummary struct {
	ID                        string
	Name                      string
	ServerUpdatedAt           time.Time
	ReadyForPermanentDeletion bool
}

var (
	// ErrNotebookNotFound deliberately covers both an unknown id and another account's id.
	ErrNotebookNotFound = errors.New("no such notebook")

	// ErrNotebookNotArchived protects a live notebook from an admin action that is only rendered
	// for tombstones. The store repeats the check because HTTP markup is not an authority boundary.
	ErrNotebookNotArchived = errors.New("notebook is not archived")

	// ErrNotebookDeletionNotSynced means at least one active device has not pulled through the
	// tombstone yet. Erasing it now would make that device miss the delete forever.
	ErrNotebookDeletionNotSynced = errors.New("notebook deletion has not reached every active device")
)

// NotebookOverviewResult is the live and archived notebook inventory shown by the admin panel.
//
// A deleted notebook is an archive entry, not a second copy of the row. Keeping the tombstone in
// notebooks is required for sync: devices that have not yet observed the delete still need to pull
// it. The two slices are merely different projections of that one durable source of truth.
type NotebookOverviewResult struct {
	NotebookCount         int64
	Notebooks             []NotebookSummary
	ArchivedNotebookCount int64
	ArchivedNotebooks     []NotebookSummary
}

// NotebookOverview counts an account's live and archived notebooks and returns up to `limit` of
// each.
//
// One query rather than separate counts and lists. The window functions are evaluated over the
// same snapshot as the rows they accompany, so neither count can disagree with its list if a sync
// lands while the dashboard is loading. Live notebooks are alphabetical; archive entries are
// newest first, which is the order in which an operator is likely to look for an accidental delete.
//
// The server receipt time is used for the archive order and label. Client wall clocks are display
// metadata only and may be wrong (syncPlan.md SD1).
func NotebookOverview(ctx context.Context, pool *pgxpool.Pool, accountID string, limit int) (NotebookOverviewResult, error) {
	const statement = `
		WITH ranked AS (
			SELECT
				id,
				name,
				server_updated_at,
				deleted_at IS NOT NULL AS archived,
				NOT EXISTS (
					SELECT 1
					FROM devices
					WHERE devices.account_id = notebooks.account_id
						AND devices.revoked_at IS NULL
						AND devices.last_pulled_seq < notebooks.change_seq
				) AS ready_for_permanent_deletion,
				count(*) OVER (PARTITION BY deleted_at IS NOT NULL) AS total,
				row_number() OVER (
					PARTITION BY deleted_at IS NOT NULL
					ORDER BY
						CASE WHEN deleted_at IS NULL THEN name END,
						CASE WHEN deleted_at IS NOT NULL THEN server_updated_at END DESC,
						id
				) AS position
			FROM notebooks
			WHERE account_id = $1::uuid
		)
		SELECT id, name, server_updated_at, archived, ready_for_permanent_deletion, total
		FROM ranked
		WHERE position <= $2
		ORDER BY archived, position`

	rows, err := pool.Query(ctx, statement, accountID, limit)
	if err != nil {
		return NotebookOverviewResult{}, fmt.Errorf("listing notebooks: %w", err)
	}
	defer rows.Close()

	overview := NotebookOverviewResult{
		Notebooks:         make([]NotebookSummary, 0),
		ArchivedNotebooks: make([]NotebookSummary, 0),
	}
	for rows.Next() {
		var summary NotebookSummary
		var archived bool
		var groupCount int64
		if err := rows.Scan(
			&summary.ID,
			&summary.Name,
			&summary.ServerUpdatedAt,
			&archived,
			&summary.ReadyForPermanentDeletion,
			&groupCount,
		); err != nil {
			return NotebookOverviewResult{}, fmt.Errorf("listing notebooks: %w", err)
		}
		if archived {
			overview.ArchivedNotebookCount = groupCount
			overview.ArchivedNotebooks = append(overview.ArchivedNotebooks, summary)
		} else {
			overview.NotebookCount = groupCount
			overview.Notebooks = append(overview.Notebooks, summary)
		}
	}
	if err := rows.Err(); err != nil {
		return NotebookOverviewResult{}, fmt.Errorf("listing notebooks: %w", err)
	}
	// With no rows in a group there is no window to read its total from, and zero is already the
	// right value in the result.
	return overview, nil
}

// DeleteArchivedNotebook permanently removes one notebook and everything beneath it.
//
// PostgreSQL owns the subtree deletion: notebooks -> sections -> pages -> page_content and every
// ink table are all ON DELETE CASCADE foreign keys. Keeping this operation as one root DELETE makes
// a newly added child table fail closed unless its migration also declares how it is deleted.
//
// The account advisory lock serialises this with sync pushes. Under that lock, the function first
// proves the row is a tombstone and then proves every active device has pulled through its change
// sequence. Revoked devices can never pull again and therefore do not block housekeeping.
func DeleteArchivedNotebook(ctx context.Context, pool *pgxpool.Pool, accountID string, notebookID string) error {
	if err := ValidateEntityID(notebookID); err != nil {
		return ErrNotebookNotFound
	}

	transaction, err := BeginAccountWrite(ctx, pool, accountID)
	if err != nil {
		return fmt.Errorf("starting permanent notebook deletion: %w", err)
	}
	defer func() { _ = transaction.Rollback(ctx) }()

	const notebookStatement = `
		SELECT deleted_at, change_seq
		FROM notebooks
		WHERE account_id = $1::uuid AND id = $2
		FOR UPDATE`

	var deletedAt *int64
	var deletionChangeSeq int64
	err = transaction.QueryRow(ctx, notebookStatement, accountID, notebookID).Scan(&deletedAt, &deletionChangeSeq)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotebookNotFound
	}
	if err != nil {
		return fmt.Errorf("reading an archived notebook before permanent deletion: %w", err)
	}
	if deletedAt == nil {
		return ErrNotebookNotArchived
	}

	const pendingDeviceStatement = `
		SELECT EXISTS (
			SELECT 1
			FROM devices
			WHERE account_id = $1::uuid
				AND revoked_at IS NULL
				AND last_pulled_seq < $2
		)`
	var hasPendingDevice bool
	if err := transaction.QueryRow(ctx, pendingDeviceStatement, accountID, deletionChangeSeq).Scan(&hasPendingDevice); err != nil {
		return fmt.Errorf("checking devices before permanent notebook deletion: %w", err)
	}
	if hasPendingDevice {
		return ErrNotebookDeletionNotSynced
	}

	result, err := transaction.Exec(ctx,
		`DELETE FROM notebooks WHERE account_id = $1::uuid AND id = $2`,
		accountID, notebookID,
	)
	if err != nil {
		return fmt.Errorf("permanently deleting an archived notebook: %w", err)
	}
	if result.RowsAffected() != 1 {
		return ErrNotebookNotFound
	}
	if err := transaction.Commit(ctx); err != nil {
		return fmt.Errorf("committing permanent notebook deletion: %w", err)
	}
	return nil
}
