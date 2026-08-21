package store

// The purge log: what this account has erased for good, and what still has to be told about it.
//
// Every other delete the protocol carries is a tombstone, which works because the row survives to
// describe itself. Permanent deletion removes the rows, so the sentence "this id is gone" has to
// live somewhere else, and that is here. See the `purges` table in migrations/ for why one table
// serves both as the queue a client drains and as the gravestone a push is refused against.

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// PurgeRow is one permanently erased entity, as a pulling client receives it.
//
// Rendered in SQL like every kind's change is, so the wire shape has exactly one definition. `seq`
// is the account sequence the purge was allocated under, so a client can order it against the
// changes in the same response; `purgedAt` is the server clock in the app's own unit.
type PurgeRow struct {
	ChangeSeq int64
	Purge     json.RawMessage
}

const purgeJSON = `jsonb_build_object(
		'kind', kind,
		'id', entity_id,
		'seq', change_seq,
		'purgedAt', (extract(epoch FROM purged_at) * 1000)::bigint)`

// RecordPurge writes the gravestone for one entity. It must be called inside BeginAccountWrite's
// transaction, with a sequence value from [AllocateChangeSeq].
//
// `ON CONFLICT DO UPDATE` rather than `DO NOTHING`: reaching an id that is already purged means the
// row came back after its purge, which this server refuses on push and therefore should not be able
// to happen. If it does, the newer sequence value is the useful one — it is the one every device
// has yet to acknowledge, and keeping the older one would let the log be pruned while a device that
// has the resurrected copy still has not heard.
func RecordPurge(ctx context.Context, transaction pgx.Tx, accountID string, kind string, entityID string, changeSeq int64) error {
	const statement = `
		INSERT INTO purges (account_id, kind, entity_id, change_seq)
		VALUES ($1::uuid, $2, $3, $4)
		ON CONFLICT (account_id, kind, entity_id) DO UPDATE SET
			change_seq = EXCLUDED.change_seq,
			purged_at = now()`

	if _, err := transaction.Exec(ctx, statement, accountID, kind, entityID, changeSeq); err != nil {
		return fmt.Errorf("recording a purge: %w", err)
	}
	return nil
}

// SelectPurges reads the purges an account recorded in (sinceCursor, upperBound].
//
// The same half-open window as [SelectDelta] and bounded by the same cursor, so "store the cursor
// once everything in this response is applied" stays one promise covering both halves of the
// response rather than two that can disagree.
//
// Deliberately uncapped, unlike the delta. A purge row is an id and two integers, and one is
// written per press of a button in the administration panel — the size of this array is the number
// of notebooks the operator erased while a device was offline, not a function of how much the
// account has written. Capping it would mean lowering the response cursor to a purge boundary, and
// a response with an empty `changes` and `hasMore` set is exactly the shape the client treats as a
// broken server.
func SelectPurges(ctx context.Context, database Querier, accountID string, sinceCursor int64, upperBound int64) ([]PurgeRow, error) {
	const statement = `
		SELECT change_seq, ` + purgeJSON + `
		FROM purges
		WHERE account_id = $1::uuid AND change_seq > $2 AND change_seq <= $3
		ORDER BY change_seq, kind, entity_id`

	rows, err := database.Query(ctx, statement, accountID, sinceCursor, upperBound)
	if err != nil {
		return nil, fmt.Errorf("reading purges: %w", err)
	}
	defer rows.Close()

	purges := make([]PurgeRow, 0)
	for rows.Next() {
		var purge PurgeRow
		if err := rows.Scan(&purge.ChangeSeq, &purge.Purge); err != nil {
			return nil, fmt.Errorf("reading purges: %w", err)
		}
		purges = append(purges, purge)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading purges: %w", err)
	}
	return purges, nil
}

// SelectPurgedIDs reports which of a push's ids this account has erased for good.
//
// The gravestone half of the log. A device that was offline when the operator pressed Permanently
// delete still holds the notebook, and if it also holds an unsent edit to it, its next push carries
// that notebook — with a stale `baseVersion`, which the version check refuses, and then with
// `baseVersion` 0, which the version check has nothing to say about. That second push would create
// the notebook again, and the account would receive its erased notebook back from the one device
// that had not heard the news. So the id is retired: nothing may be stored under it again.
func SelectPurgedIDs(ctx context.Context, database Querier, accountID string, kind string, entityIDs []string) (map[string]struct{}, error) {
	purged := make(map[string]struct{})
	if len(entityIDs) == 0 {
		return purged, nil
	}

	const statement = `
		SELECT entity_id
		FROM purges
		WHERE account_id = $1::uuid AND kind = $2 AND entity_id = ANY($3::text[])`

	rows, err := database.Query(ctx, statement, accountID, kind, entityIDs)
	if err != nil {
		return nil, fmt.Errorf("checking purged %s ids: %w", kind, err)
	}
	defer rows.Close()

	for rows.Next() {
		var entityID string
		if err := rows.Scan(&entityID); err != nil {
			return nil, fmt.Errorf("checking purged %s ids: %w", kind, err)
		}
		purged[entityID] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("checking purged %s ids: %w", kind, err)
	}
	return purged, nil
}

// PrunePurges drops the gravestones every active device has already pulled past.
//
// This is the condition that used to stand between the operator and the Permanently delete button.
// It is the right rule and it was in the wrong place: as a precondition it made an offline tablet
// able to veto the account owner's housekeeping indefinitely, whereas as a retention rule it just
// decides when a hundred bytes of log stop being useful. A device that has acknowledged a cursor
// above `change_seq` has both received the purge and committed it (see [RecordAcknowledgedSeq]),
// and can never push the id again either, because it no longer holds the row.
//
// A revoked device does not hold it back: it can never pull again, and it can never push again
// either, so nothing is waiting for it. An account with no active devices at all prunes
// immediately, which is correct for the same reason and not a special case.
//
// Called from the permanent deletion that writes the next purge rather than from a background
// sweeper: the set is small, the delete is indexed by the primary key's leading column, and a
// sweeper is one more thing that can be forgotten in a self-hosted deployment (the same argument
// [RecordAppliedBatchResponse] makes for pruning inline).
func PrunePurges(ctx context.Context, transaction pgx.Tx, accountID string) error {
	const statement = `
		DELETE FROM purges
		WHERE account_id = $1::uuid
			AND NOT EXISTS (
				SELECT 1
				FROM devices
				WHERE devices.account_id = purges.account_id
					AND devices.revoked_at IS NULL
					AND devices.last_pulled_seq < purges.change_seq
			)`

	if _, err := transaction.Exec(ctx, statement, accountID); err != nil {
		return fmt.Errorf("pruning purges: %w", err)
	}
	return nil
}
