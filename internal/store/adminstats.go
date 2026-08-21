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
	ID              string
	Name            string
	ServerUpdatedAt time.Time

	// Closed is whether the notebook is off the app's rail. Shown because it is the difference
	// between a notebook nobody has opened lately and one the account has deliberately shelved —
	// and because it is what decides whether a client's delete erases this row or keeps it here.
	Closed bool

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

// Which shelf a notebook is on, as the overview query numbers them. The numbers are the display
// order as well, which is why they are ordinals rather than a string: synced notebooks first, then
// the ones only this server still holds, then the tombstones.
const (
	notebookShelfSynced   = 0
	notebookShelfOnCloud  = 1
	notebookShelfArchived = 2
)

// NotebookOverviewResult is the synced, cloud-hosted and archived notebook inventory shown by the
// admin panel.
//
// Three projections of one table, not three tables. A deleted notebook is an archive entry rather
// than a second copy of the row: keeping the tombstone in `notebooks` is required for sync, because
// devices that have not yet observed the delete still need to pull it. A cloud-hosted notebook is
// the opposite case and just as much the same row — it is live, it is pulled like any other, and
// what makes it its own group is that no device holds its contents any more, so this server is the
// only place they exist.
type NotebookOverviewResult struct {
	NotebookCount         int64
	Notebooks             []NotebookSummary
	CloudNotebookCount    int64
	CloudNotebooks        []NotebookSummary
	ArchivedNotebookCount int64
	ArchivedNotebooks     []NotebookSummary
}

// notebookOverviewStatement is built once from the shelf constants above, the same way the delta
// query is built from the kind registry: the numbers the query partitions by and the numbers the
// scan switches on are then one definition rather than two that have to be kept level by hand.
//
// The `shelved` CTE exists so the shelf rule is written once. A window function cannot see a select
// alias, so without it the CASE would be repeated in the partition, in the ordering and in the
// output — three copies of one rule, and nothing to tell you when they stop agreeing.
var notebookOverviewStatement = fmt.Sprintf(`
	WITH shelved AS (
		SELECT
			account_id,
			id,
			name,
			server_updated_at,
			change_seq,
			closed_at IS NOT NULL AS closed,
			CASE
				WHEN deleted_at IS NOT NULL THEN %[1]d
				WHEN cloud_only_at IS NOT NULL THEN %[2]d
				ELSE %[3]d
			END AS shelf
		FROM notebooks
		WHERE account_id = $1::uuid
	), ranked AS (
		SELECT
			id,
			name,
			server_updated_at,
			closed,
			shelf,
			NOT EXISTS (
				SELECT 1
				FROM devices
				WHERE devices.account_id = shelved.account_id
					AND devices.revoked_at IS NULL
					AND devices.last_pulled_seq < shelved.change_seq
			) AS ready_for_permanent_deletion,
			count(*) OVER (PARTITION BY shelf) AS total,
			row_number() OVER (
				PARTITION BY shelf
				ORDER BY
					CASE WHEN shelf <> %[1]d THEN name END,
					CASE WHEN shelf = %[1]d THEN server_updated_at END DESC,
					id
			) AS position
		FROM shelved
	)
	SELECT id, name, server_updated_at, closed, shelf, ready_for_permanent_deletion, total
	FROM ranked
	WHERE position <= $2
	ORDER BY shelf, position`,
	notebookShelfArchived, notebookShelfOnCloud, notebookShelfSynced)

// NotebookOverview counts an account's synced, cloud-hosted and archived notebooks and returns up
// to `limit` of each.
//
// One query rather than three counts and three lists. The window functions are evaluated over the
// same snapshot as the rows they accompany, so no count can disagree with its list if a sync lands
// while the dashboard is loading. Live notebooks are alphabetical; archive entries are newest
// first, which is the order in which an operator is likely to look for an accidental delete.
//
// The server receipt time is used for the archive order and label. Client wall clocks are display
// metadata only and may be wrong (syncPlan.md SD1).
func NotebookOverview(ctx context.Context, pool *pgxpool.Pool, accountID string, limit int) (NotebookOverviewResult, error) {
	rows, err := pool.Query(ctx, notebookOverviewStatement, accountID, limit)
	if err != nil {
		return NotebookOverviewResult{}, fmt.Errorf("listing notebooks: %w", err)
	}
	defer rows.Close()

	overview := NotebookOverviewResult{
		Notebooks:         make([]NotebookSummary, 0),
		CloudNotebooks:    make([]NotebookSummary, 0),
		ArchivedNotebooks: make([]NotebookSummary, 0),
	}
	for rows.Next() {
		var summary NotebookSummary
		var shelf int
		var groupCount int64
		if err := rows.Scan(
			&summary.ID,
			&summary.Name,
			&summary.ServerUpdatedAt,
			&summary.Closed,
			&shelf,
			&summary.ReadyForPermanentDeletion,
			&groupCount,
		); err != nil {
			return NotebookOverviewResult{}, fmt.Errorf("listing notebooks: %w", err)
		}
		switch shelf {
		case notebookShelfArchived:
			overview.ArchivedNotebookCount = groupCount
			overview.ArchivedNotebooks = append(overview.ArchivedNotebooks, summary)
		case notebookShelfOnCloud:
			overview.CloudNotebookCount = groupCount
			overview.CloudNotebooks = append(overview.CloudNotebooks, summary)
		default:
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

// ErrNotebookNotCloudHosted refuses to unhost a notebook this server is not the last copy of.
//
// It covers a live notebook, whose contents are on the devices and whose deletion is therefore
// theirs to ask for, and one that is already archived, where the tombstone this would write already
// exists. Both are the same mistake seen from either side: the markup that offers the action is
// stale, and markup is not an authority boundary.
var ErrNotebookNotCloudHosted = errors.New("notebook is not hosted only on this server")

// StopHostingCloudNotebook deletes a cloud-hosted notebook by writing an ordinary tombstone into
// the account's change stream.
//
// This is the one delete a device cannot ask for. A closed notebook's tombstone is refused by
// [NotebookFields.RetainOnDelete] precisely because the device pushing it no longer holds the
// contents — which leaves the operator, who can see how much disk the thing occupies, as the only
// party in a position to say it should go. Reopening it on a device is the other way, and it means
// downloading the whole notebook in order to throw it away.
//
// A tombstone rather than a DELETE, for the reason every delete in this schema is a tombstone: the
// devices still hold the notebook's skeleton — its sections and pages, which a cloud-only notebook
// deliberately leaves behind — and a row that is simply gone cannot be described to them. So this
// takes the ordinary path instead: the account's advisory lock, a sequence value of its own, a
// version bump, and the row goes on to appear under Archived, where the existing interlock waits
// for every active device to acknowledge the deletion before anything is erased for good.
//
// `last_writer` is cleared rather than attributed. No device wrote this, and naming one would put a
// lie in the only column that answers "which of my tablets did that?".
func StopHostingCloudNotebook(ctx context.Context, pool *pgxpool.Pool, accountID string, notebookID string) error {
	if err := ValidateEntityID(notebookID); err != nil {
		return ErrNotebookNotFound
	}

	transaction, err := BeginAccountWrite(ctx, pool, accountID)
	if err != nil {
		return fmt.Errorf("starting a cloud notebook deletion: %w", err)
	}
	defer func() { _ = transaction.Rollback(ctx) }()

	const notebookStatement = `
		SELECT deleted_at, cloud_only_at
		FROM notebooks
		WHERE account_id = $1::uuid AND id = $2
		FOR UPDATE`

	var deletedAt *int64
	var cloudOnlyAt *int64
	err = transaction.QueryRow(ctx, notebookStatement, accountID, notebookID).Scan(&deletedAt, &cloudOnlyAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotebookNotFound
	}
	if err != nil {
		return fmt.Errorf("reading a cloud notebook before deleting it: %w", err)
	}
	if deletedAt != nil || cloudOnlyAt == nil {
		return ErrNotebookNotCloudHosted
	}

	allocatedSeq, err := AllocateChangeSeq(ctx, transaction, accountID)
	if err != nil {
		return err
	}

	// The app's own clock unit. `updated_at` moves with it because it is what the app displays as
	// the time of the last change, and this is one.
	deletedAtMillis := time.Now().UnixMilli()

	const tombstoneStatement = `
		UPDATE notebooks
		SET deleted_at = $3,
			updated_at = $3,
			version = version + 1,
			change_seq = $4,
			server_updated_at = now(),
			last_writer = NULL
		WHERE account_id = $1::uuid AND id = $2`

	result, err := transaction.Exec(ctx, tombstoneStatement, accountID, notebookID, deletedAtMillis, allocatedSeq)
	if err != nil {
		return fmt.Errorf("deleting a cloud notebook: %w", err)
	}
	if result.RowsAffected() != 1 {
		// Unreachable: the row was locked FOR UPDATE two statements ago.
		return ErrNotebookNotFound
	}
	if err := transaction.Commit(ctx); err != nil {
		return fmt.Errorf("committing a cloud notebook deletion: %w", err)
	}
	return nil
}
