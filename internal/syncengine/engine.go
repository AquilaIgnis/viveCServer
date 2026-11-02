package syncengine

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AquilaIgnis/viveCServer/internal/auth"
	"github.com/AquilaIgnis/viveCServer/internal/store"
)

// PushRequest is one client push, already read off the wire.
type PushRequest struct {
	// Caller comes from the authentication middleware and never from the body. It is the whole of
	// this server's tenancy model.
	Caller auth.AuthenticatedDevice

	// BatchID is the client's idempotency key: the same value on every retry of one batch.
	BatchID string

	// Changes are the raw JSON objects, kept raw so unrecognised fields survive (SD5).
	Changes []json.RawMessage

	// ReplayWindow is how long a batch's answer stays replayable.
	ReplayWindow time.Duration
}

// PushResult is what a push answers.
//
// It is marshalled once and then both returned to the caller and stored in `applied_batches`, so a
// retry replays exactly the bytes the first attempt produced.
type PushResult struct {
	Applied  []AppliedChange  `json:"applied"`
	Rejected []RejectedChange `json:"rejected"`
	Cursor   int64            `json:"cursor"`
}

// PullResult is what a delta request answers.
type PullResult struct {
	Changes []json.RawMessage `json:"changes"`

	// Purges are the entities this account erased for good inside the same window as Changes: ids
	// with nothing left to describe them, which a client deletes locally along with everything
	// beneath them. Always present, usually empty.
	//
	// A second array rather than more entries in Changes. A purge is not a change and could not
	// pretend to be one — no version, no baseVersion, no parent, no push path — so a ninth
	// store.SyncKind would be a fake table and four interface methods answering nothing.
	//
	// It is also the shape that survives the server and the app being deployed separately. A client
	// that does not know this field ignores it and keeps a notebook it should have dropped, which is
	// untidy; a client that met an unrecognised `kind` inside Changes would stop synchronising
	// altogether rather than advance a cursor past a row it could not store, which is the right
	// thing for it to do and the wrong thing to provoke every time this container restarts ahead of
	// the tablets.
	Purges []json.RawMessage `json:"purges"`

	Cursor  int64 `json:"cursor"`
	HasMore bool  `json:"hasMore"`
}

// ApplyChangeBatch stores what it can of a push and reports what it refused.
//
// The whole batch runs in one transaction under the account's advisory lock, so the sequence value
// it allocates is also its commit order (SD2), and a reader can never consume a cursor while an
// earlier change is still invisible.
//
// Returns the marshalled response rather than the struct: those exact bytes are what a retry has to
// replay, and re-encoding them later would let the two answers drift. The boolean is true only when
// this call committed new rows, so the HTTP layer never broadcasts an idempotent replay.
func ApplyChangeBatch(ctx context.Context, pool *pgxpool.Pool, request PushRequest) (json.RawMessage, bool, error) {
	transaction, err := store.BeginAccountWrite(ctx, pool, request.Caller.AccountID)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = transaction.Rollback(ctx) }()

	// Inside the lock, so two retries of one batch cannot both conclude that they are the first.
	storedResponse, alreadyApplied, err := store.FindAppliedBatchResponse(
		ctx, transaction, request.Caller.DeviceID, request.BatchID, request.ReplayWindow,
	)
	if err != nil {
		return nil, false, err
	}
	if alreadyApplied {
		return storedResponse, false, nil
	}

	decoded := decodeChanges(request.Changes)
	applied := make([]AppliedChange, 0, len(decoded))
	rejected := make([]RejectedChange, 0)
	pendingWrites := make([]store.PendingWrite, 0, len(decoded))

	// Which ids this batch has already accepted, per kind, so a child can find a parent that is
	// arriving in the same push.
	acceptedIDsByKind := make(map[string]map[string]struct{}, len(decoded))

	// decodeChanges sorted by kind, so each run below is one kind and each kind's database reads
	// happen once for the whole run rather than once per entity.
	for runStart := 0; runStart < len(decoded); {
		runEnd := runStart + 1
		for runEnd < len(decoded) && decoded[runEnd].kind == decoded[runStart].kind {
			runEnd++
		}
		run := decoded[runStart:runEnd]
		runStart = runEnd

		kind := run[0].kind
		if kind == nil {
			// Changes whose kind could not be resolved. They were refused while being read.
			for _, change := range run {
				rejected = append(rejected, *change.rejection)
			}
			continue
		}

		batchContext, err := readBatchContext(ctx, transaction, request.Caller.AccountID, kind, run)
		if err != nil {
			return nil, false, err
		}

		for _, change := range run {
			if change.rejection != nil {
				rejected = append(rejected, *change.rejection)
				continue
			}

			if kind.ParentKind != "" {
				parentID := change.fields.ParentID()
				_, storedParent := batchContext.ExistingParentIDs[parentID]
				_, parentInThisBatch := acceptedIDsByKind[kind.ParentKind][parentID]
				if !storedParent && !parentInThisBatch {
					rejected = append(rejected, RejectedChange{
						Kind:   kind.Name,
						ID:     change.envelope.ID,
						Reason: ReasonMissingParent,
						Message: fmt.Sprintf("%s %q has not been uploaded to this account",
							kind.ParentKind, parentID),
					})
					continue
				}
			}

			// SD7: the server refuses to hold a reference to bytes it does not have, which is what
			// makes "attachments upload before the change referencing them" an invariant instead of
			// a convention. The client uploads and pushes this entity again.
			//
			// Only a live entity is held to it. A tombstone naming a blob that has already been
			// swept is exactly how a picture stops existing, and refusing that delete would leave a
			// row the client can never retract.
			if change.envelope.DeletedAt == nil && len(change.requiredBlobs) > 0 {
				if missing := missingDigests(change.requiredBlobs, batchContext.StoredBlobs); len(missing) > 0 {
					rejected = append(rejected, RejectedChange{
						Kind:    kind.Name,
						ID:      change.envelope.ID,
						Reason:  ReasonMissingBlob,
						Message: describeMissingBlobs(missing),
					})
					continue
				}

				if checker, checksBlobs := change.fields.(store.BlobConsistencyChecker); checksBlobs {
					if err := checker.CheckBlobPresence(change.envelope.ID, batchContext.StoredBlobs); err != nil {
						rejected = append(rejected, RejectedChange{
							Kind:    kind.Name,
							ID:      change.envelope.ID,
							Reason:  ReasonMalformed,
							Message: err.Error(),
						})
						continue
					}
				}
			}

			// An id the account erased for good. Refused before the version check, because the
			// version check has nothing useful to say about it: the row is gone, so a stale
			// baseVersion is a conflict with no current state to send back, and the client's answer
			// to that is to re-push at baseVersion 0 — which would create the notebook again. The
			// device that was offline when the operator pressed Permanently delete would then be the
			// one device able to undo it. See [store.ChangeContext.PurgedIDs].
			if _, retired := batchContext.PurgedIDs[change.envelope.ID]; retired {
				rejected = append(rejected, RejectedChange{
					Kind:    kind.Name,
					ID:      change.envelope.ID,
					Reason:  ReasonPurged,
					Message: "this account permanently deleted this entity; delete your copy of it",
				})
				continue
			}

			current, storedHere := batchContext.CurrentByID[change.envelope.ID]
			switch {
			case storedHere && current.Version != change.baseVersion:
				// The entity moved on since the client last saw it. The current row travels with
				// the rejection so the client can merge or take it without asking again.
				rejected = append(rejected, RejectedChange{
					Kind:   kind.Name,
					ID:     change.envelope.ID,
					Reason: ReasonVersionConflict,
					Message: fmt.Sprintf("the stored version is %d, not %d",
						current.Version, change.baseVersion),
					Current: current.Change,
				})
				continue

			case !storedHere && change.baseVersion != 0:
				// The client is editing something this server has never held. Accepting it would
				// silently resurrect a row whose history the server cannot describe, so it is
				// refused with no current state and the client re-pushes it as new.
				rejected = append(rejected, RejectedChange{
					Kind:    kind.Name,
					ID:      change.envelope.ID,
					Reason:  ReasonVersionConflict,
					Message: "this account has no such entity; push it with baseVersion 0",
				})
				continue
			}

			envelope := change.envelope
			envelope.AccountID = request.Caller.AccountID
			envelope.LastWriter = request.Caller.DeviceID
			envelope.Version = change.baseVersion + 1

			// A tombstone a kind would rather keep. Only `notebook` implements this, and only for a
			// notebook that is closed: the delete stops being an erasure and becomes the account
			// saying no device holds this any more (store.NotebookFields.RetainOnDelete). The
			// entity still counts as applied and still gets a version, because the write happened —
			// what the client asked for and what was stored simply differ, and the next pull is
			// where the client learns that, exactly as it would for any row another device moved.
			if envelope.DeletedAt != nil {
				if retainer, retainable := change.fields.(store.DeletionRetainer); retainable {
					if retainer.RetainOnDelete(current.Change, *envelope.DeletedAt) {
						envelope.DeletedAt = nil
					}
				}
			}

			pendingWrites = append(pendingWrites, store.PendingWrite{
				Kind:        kind.Name,
				Envelope:    envelope,
				Fields:      change.fields,
				BaseVersion: change.baseVersion,
			})
			applied = append(applied, AppliedChange{
				Kind:    kind.Name,
				ID:      envelope.ID,
				Version: envelope.Version,
			})
			if acceptedIDsByKind[kind.Name] == nil {
				acceptedIDsByKind[kind.Name] = make(map[string]struct{})
			}
			acceptedIDsByKind[kind.Name][envelope.ID] = struct{}{}
		}
	}

	cursor := int64(0)
	if len(pendingWrites) > 0 {
		// Allocated only now, so a batch that was refused in full burns no sequence number and
		// emits no change-feed wakeup to the account's other devices.
		allocatedSeq, err := store.AllocateChangeSeq(ctx, transaction, request.Caller.AccountID)
		if err != nil {
			return nil, false, err
		}
		for index := range pendingWrites {
			pendingWrites[index].Envelope.ChangeSeq = allocatedSeq
		}
		if err := store.WriteChanges(ctx, transaction, pendingWrites); err != nil {
			return nil, false, err
		}
		cursor = allocatedSeq
	} else {
		cursor, err = store.ReadChangeSeq(ctx, transaction, request.Caller.AccountID)
		if err != nil {
			return nil, false, err
		}
	}

	response, err := json.Marshal(PushResult{Applied: applied, Rejected: rejected, Cursor: cursor})
	if err != nil {
		return nil, false, fmt.Errorf("encoding a push response: %w", err)
	}

	if err := store.RecordAppliedBatchResponse(
		ctx, transaction, request.Caller.DeviceID, request.BatchID, response, request.ReplayWindow,
	); err != nil {
		return nil, false, err
	}

	if err := transaction.Commit(ctx); err != nil {
		return nil, false, fmt.Errorf("committing a push: %w", err)
	}
	return response, len(pendingWrites) > 0, nil
}

// readBatchContext gathers what one kind's run needs and asks for all of it at once.
//
// Four questions rather than four per entity: a push carries up to 512 changes, and a round trip
// each would cost more than the writes they guard. [store.ReadChangeContext] pipelines them, since
// nothing here depends on anything else here.
func readBatchContext(
	ctx context.Context,
	transaction pgx.Tx,
	accountID string,
	kind *store.SyncKind,
	run []decodedChange,
) (store.ChangeContext, error) {
	entityIDs := make([]string, 0, len(run))
	parentIDs := make([]string, 0, len(run))
	requiredDigests := make([]string, 0)
	seenDigests := make(map[string]struct{})
	for _, change := range run {
		if change.rejection != nil {
			continue
		}
		entityIDs = append(entityIDs, change.envelope.ID)
		if kind.ParentKind != "" {
			parentIDs = append(parentIDs, change.fields.ParentID())
		}
		if change.envelope.DeletedAt != nil {
			continue
		}
		for _, digest := range change.requiredBlobs {
			// Deduplicated across the run: one picture on twenty pages is one array element, not
			// twenty, and the same document pushed with repeated refs cannot inflate the query.
			if _, seen := seenDigests[digest]; seen {
				continue
			}
			seenDigests[digest] = struct{}{}
			requiredDigests = append(requiredDigests, digest)
		}
	}

	var parentKind *store.SyncKind
	if kind.ParentKind != "" {
		resolved, known := store.SyncKindByName(kind.ParentKind)
		if !known {
			return store.ChangeContext{}, fmt.Errorf("kind %q names an unregistered parent kind %q", kind.Name, kind.ParentKind)
		}
		parentKind = resolved
	}

	return store.ReadChangeContext(
		ctx, transaction, kind, parentKind, accountID, entityIDs, parentIDs, requiredDigests,
	)
}

// missingDigests reports which of an entity's required blobs this account does not hold.
func missingDigests(required []string, stored map[string]store.BlobPresence) []string {
	missing := make([]string, 0)
	for _, digest := range required {
		if _, held := stored[digest]; !held {
			missing = append(missing, digest)
		}
	}
	return missing
}

// describeMissingBlobs names what has to be uploaded.
//
// Every digest, not a count: the client's next move is to upload exactly these, and a message that
// said "3 attachments are missing" would make it diff its own reference set to find out which.
// Bounded because a rejection is not a place to return a page of data — the client can re-push and
// be told about the rest, and a document naming 512 pictures none of which are stored is a broken
// client rather than a case to optimise for.
func describeMissingBlobs(missing []string) string {
	const named = 8

	listed := missing
	suffix := ""
	if len(listed) > named {
		listed = listed[:named]
		suffix = fmt.Sprintf(" and %d more", len(missing)-named)
	}
	return "upload these attachments before pushing this change: " + strings.Join(listed, ", ") + suffix
}

// PullChanges returns the changes an account accumulated after a client's cursor, together with
// the ids it erased for good in the same window.
//
// Both halves are bounded by the cursor this response returns, not by the account's current one, so
// "store the cursor once everything above it has been applied" stays a single promise. A page that
// stopped short leaves the rest of the purges to the pull that follows it, and a purge committed
// between the two reads takes a sequence value above the bound and is picked up next time — the
// same argument [selectChangePage] makes for reading the cursor before the delta.
func PullChanges(
	ctx context.Context,
	pool *pgxpool.Pool,
	accountID string,
	sinceCursor int64,
	limit int,
) (PullResult, error) {
	result, err := selectChangePage(ctx, pool, accountID, sinceCursor, limit)
	if err != nil {
		return PullResult{}, err
	}

	purges, err := store.SelectPurges(ctx, pool, accountID, sinceCursor, result.Cursor)
	if err != nil {
		return PullResult{}, err
	}
	result.Purges = make([]json.RawMessage, 0, len(purges))
	for _, purge := range purges {
		result.Purges = append(result.Purges, purge.Purge)
	}
	return result, nil
}

// selectChangePage is the delta half of a pull: everything but the purges.
//
// The cursor is read first and the delta is then bounded by it. Doing it the other way round would
// let a push commit between the two reads, and the cursor returned would sit above rows the client
// never received — which is the same lost-update the advisory lock exists to prevent, reintroduced
// on the read side.
//
// Two queries, and the cheap one decides what the expensive one reads: the first finds the sequence
// this page ends at from row counts and size estimates, the second renders exactly that range. See
// [store.SelectDeltaCutoff]. Because the boundary is what comes back, there is no over-read by one
// row, no truncation to a sequence boundary, and no second query for an oversized push.
//
// The two queries run in separate snapshots, which is safe because rows only ever leave a range
// that is already below the cursor: a write gives its row a sequence above upperBound. A row that
// moved between the two reads is missing from this page and delivered at its new sequence by the
// next one, which is also what a single-query read taken a moment later would have returned.
func selectChangePage(
	ctx context.Context,
	pool *pgxpool.Pool,
	accountID string,
	sinceCursor int64,
	limit int,
) (PullResult, error) {
	upperBound, err := store.ReadChangeSeq(ctx, pool, accountID)
	if err != nil {
		return PullResult{}, err
	}

	caughtUp := PullResult{Changes: make([]json.RawMessage, 0), Cursor: upperBound, HasMore: false}
	if sinceCursor >= upperBound {
		// Includes the client that is ahead of the server, which a restore from backup can produce.
		// S6 turns that case into `410 cursor_expired` and a full reconcile; until the client can
		// act on it, the honest cursor is more useful than an error it would only log.
		return caughtUp, nil
	}

	cutoff, err := store.SelectDeltaCutoff(
		ctx, pool, accountID, sinceCursor, upperBound, limit, store.MaxDeltaBytes,
	)
	if err != nil {
		return PullResult{}, err
	}
	if cutoff.Empty {
		// Sequence values the account allocated but this range has no rows for — every row of a
		// permanently deleted notebook, say. Returning the account's own value rather than a row's
		// is what stops the client asking again for a gap that will never have anything in it.
		return caughtUp, nil
	}

	// Complete means nothing was held back, so the page reaches the bound the caller asked about
	// and the cursor is that bound rather than the last row's sequence — for the same reason.
	pageEnd := cutoff.Sequence
	if cutoff.Complete {
		pageEnd = upperBound
	}

	delta, bytesRead, err := store.SelectDelta(ctx, pool, accountID, sinceCursor, pageEnd)
	if err != nil {
		return PullResult{}, err
	}
	if err := checkDeltaWithinBudget(delta, bytesRead); err != nil {
		return PullResult{}, err
	}

	return PullResult{
		Changes: changesOf(delta),
		Cursor:  pageEnd,
		HasMore: pageEnd < upperBound,
	}, nil
}

// checkDeltaWithinBudget holds the boundary query to its promise.
//
// A page spanning more than one sequence value is one [store.SelectDeltaCutoff] chose the size of,
// so a large overrun means its estimates no longer describe what the render produces — what happens
// when a kind gains a column and only its changeJSON is updated. Failing here makes that one loud
// error rather than responses nothing bounds.
//
// A single-sequence page is exempt at any size: a sequence is atomic across kinds, so a push larger
// than the requested page is returned whole or the account can never pull past it.
func checkDeltaWithinBudget(delta []store.DeltaRow, bytesRead int) error {
	const ceiling = store.MaxDeltaBytes * store.DeltaBytesOverrunFactor
	if bytesRead <= ceiling || len(delta) == 0 {
		return nil
	}
	if delta[0].ChangeSeq == delta[len(delta)-1].ChangeSeq {
		return nil
	}
	return fmt.Errorf(
		"a delta page spanning sequences %d to %d rendered %d bytes, past the %d ceiling: a kind's byteWeight no longer matches its changeJSON",
		delta[0].ChangeSeq, delta[len(delta)-1].ChangeSeq, bytesRead, ceiling,
	)
}

func changesOf(delta []store.DeltaRow) []json.RawMessage {
	changes := make([]json.RawMessage, 0, len(delta))
	for _, row := range delta {
		changes = append(changes, row.Change)
	}
	return changes
}
