package store

import (
	"context"
	"fmt"
	"time"

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
}

// NotebookOverview counts an account's live notebooks and returns up to `limit` of them by name.
//
// One query rather than a count followed by a list. `count(*) OVER ()` is evaluated over the same
// snapshot as the rows it accompanies, so the number on the tile can never disagree with the list
// underneath it — which it could if a sync landed between two separate statements.
//
// Tombstones are excluded. A deleted notebook stays in the table so a device that has not pulled
// yet can still be told it is gone (migrations/00003_sync_hierarchy.sql), but it is not a notebook
// the operator has, and counting it would make the dashboard disagree with every client.
func NotebookOverview(ctx context.Context, pool *pgxpool.Pool, accountID string, limit int) (int64, []NotebookSummary, error) {
	const statement = `
		SELECT id, name, server_updated_at, count(*) OVER () AS total
		FROM notebooks
		WHERE account_id = $1::uuid AND deleted_at IS NULL
		ORDER BY name, id
		LIMIT $2`

	rows, err := pool.Query(ctx, statement, accountID, limit)
	if err != nil {
		return 0, nil, fmt.Errorf("listing notebooks: %w", err)
	}
	defer rows.Close()

	var notebookCount int64
	summaries := make([]NotebookSummary, 0)
	for rows.Next() {
		var summary NotebookSummary
		if err := rows.Scan(&summary.ID, &summary.Name, &summary.ServerUpdatedAt, &notebookCount); err != nil {
			return 0, nil, fmt.Errorf("listing notebooks: %w", err)
		}
		summaries = append(summaries, summary)
	}
	if err := rows.Err(); err != nil {
		return 0, nil, fmt.Errorf("listing notebooks: %w", err)
	}
	// With no rows there is no window to read the total from, and zero is the right answer anyway.
	return notebookCount, summaries, nil
}
