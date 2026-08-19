package syncengine

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

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
	Cursor  int64             `json:"cursor"`
	HasMore bool              `json:"hasMore"`
}

// ApplyChangeBatch stores what it can of a push and reports what it refused.
//
// The whole batch runs in one transaction under the account's advisory lock, so the sequence value
// it allocates is also its commit order (SD2), and a reader can never consume a cursor while an
// earlier change is still invisible.
//
// Returns the marshalled response rather than the struct: those exact bytes are what a retry has to
// replay, and re-encoding them later would let the two answers drift.
func ApplyChangeBatch(ctx context.Context, pool *pgxpool.Pool, request PushRequest) (json.RawMessage, error) {
	transaction, err := store.BeginAccountWrite(ctx, pool, request.Caller.AccountID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = transaction.Rollback(ctx) }()

	// Inside the lock, so two retries of one batch cannot both conclude that they are the first.
	storedResponse, alreadyApplied, err := store.FindAppliedBatchResponse(
		ctx, transaction, request.Caller.DeviceID, request.BatchID, request.ReplayWindow,
	)
	if err != nil {
		return nil, err
	}
	if alreadyApplied {
		return storedResponse, nil
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
			return nil, err
		}

		for _, change := range run {
			if change.rejection != nil {
				rejected = append(rejected, *change.rejection)
				continue
			}

			if kind.ParentKind != "" {
				parentID := change.fields.ParentID()
				_, storedParent := batchContext.parentIDs[parentID]
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
				if missing := missingDigests(change.requiredBlobs, batchContext.storedBlobs); len(missing) > 0 {
					rejected = append(rejected, RejectedChange{
						Kind:    kind.Name,
						ID:      change.envelope.ID,
						Reason:  ReasonMissingBlob,
						Message: describeMissingBlobs(missing),
					})
					continue
				}

				if checker, checksBlobs := change.fields.(store.BlobConsistencyChecker); checksBlobs {
					if err := checker.CheckBlobPresence(change.envelope.ID, batchContext.storedBlobs); err != nil {
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

			current, storedHere := batchContext.currentByID[change.envelope.ID]
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
		// leaves every other device's idle poll with nothing to fetch.
		allocatedSeq, err := store.AllocateChangeSeq(ctx, transaction, request.Caller.AccountID)
		if err != nil {
			return nil, err
		}
		for index := range pendingWrites {
			pendingWrites[index].Envelope.ChangeSeq = allocatedSeq
		}
		if err := store.WriteChanges(ctx, transaction, pendingWrites); err != nil {
			return nil, err
		}
		cursor = allocatedSeq
	} else {
		cursor, err = store.ReadChangeSeq(ctx, transaction, request.Caller.AccountID)
		if err != nil {
			return nil, err
		}
	}

	response, err := json.Marshal(PushResult{Applied: applied, Rejected: rejected, Cursor: cursor})
	if err != nil {
		return nil, fmt.Errorf("encoding a push response: %w", err)
	}

	if err := store.RecordAppliedBatchResponse(
		ctx, transaction, request.Caller.DeviceID, request.BatchID, response, request.ReplayWindow,
	); err != nil {
		return nil, err
	}

	if err := transaction.Commit(ctx); err != nil {
		return nil, fmt.Errorf("committing a push: %w", err)
	}
	return response, nil
}

// batchContext is everything one kind's run needs from the database, read once for the whole run.
type batchContext struct {
	// currentByID holds the stored rows the run is editing, for the version check.
	currentByID map[string]store.CurrentChange

	// parentIDs is which of the parents the run names already exist.
	parentIDs map[string]struct{}

	// storedBlobs is which of the attachment digests the run references are held by this account,
	// and how large each one is.
	storedBlobs map[string]store.BlobPresence
}

// readBatchContext fetches all of that in at most three queries.
//
// Three rather than three per entity: a push carries up to 512 changes, and a round trip each would
// make the checks cost more than the writes they guard. Each query takes the run's ids as an array.
func readBatchContext(
	ctx context.Context,
	database store.Querier,
	accountID string,
	kind *store.SyncKind,
	run []decodedChange,
) (batchContext, error) {
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

	currentByID, err := store.SelectCurrentChanges(ctx, database, kind, accountID, entityIDs)
	if err != nil {
		return batchContext{}, err
	}

	existingParentIDs := map[string]struct{}{}
	if kind.ParentKind != "" {
		parentKind, known := store.SyncKindByName(kind.ParentKind)
		if !known {
			return batchContext{}, fmt.Errorf("kind %q names an unregistered parent kind %q", kind.Name, kind.ParentKind)
		}
		existingParentIDs, err = store.SelectExistingIDs(ctx, database, parentKind, accountID, parentIDs)
		if err != nil {
			return batchContext{}, err
		}
	}

	storedBlobs, err := store.SelectStoredBlobs(ctx, database, accountID, requiredDigests)
	if err != nil {
		return batchContext{}, err
	}

	return batchContext{currentByID: currentByID, parentIDs: existingParentIDs, storedBlobs: storedBlobs}, nil
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

// PullChanges returns the changes an account accumulated after a client's cursor.
//
// The cursor is read first and the delta is then bounded by it. Doing it the other way round would
// let a push commit between the two reads, and the cursor returned would sit above rows the client
// never received — which is the same lost-update the advisory lock exists to prevent, reintroduced
// on the read side.
func PullChanges(
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

	empty := PullResult{Changes: make([]json.RawMessage, 0), Cursor: upperBound, HasMore: false}
	if sinceCursor >= upperBound {
		// Includes the client that is ahead of the server, which a restore from backup can produce.
		// S6 turns that case into `410 cursor_expired` and a full reconcile; until the client can
		// act on it, the honest cursor is more useful than an error it would only log.
		return empty, nil
	}

	// One row past the limit, purely to learn whether there are more.
	delta, stoppedForBytes, err := store.SelectDelta(ctx, pool, accountID, sinceCursor, upperBound, limit+1)
	if err != nil {
		return PullResult{}, err
	}
	if len(delta) <= limit && !stoppedForBytes {
		return PullResult{Changes: changesOf(delta), Cursor: upperBound, HasMore: false}, nil
	}
	if len(delta) <= limit {
		// Fewer rows than the client asked for, and still not everything: the delta ran into the
		// byte budget instead. It already ends where a sequence value does, so the last one read is
		// the cursor and the client comes back for the rest.
		cursor := delta[len(delta)-1].ChangeSeq
		return PullResult{Changes: changesOf(delta), Cursor: cursor, HasMore: cursor < upperBound}, nil
	}

	// There is more than one page of delta, so the response has to stop at a sequence boundary. A
	// batch is atomic across kinds (SD2) — a page and the section it was moved into are stamped with
	// one value — so a cursor that landed inside a sequence value would let a client acknowledge
	// half of a push and never see the other half.
	overflowSeq := delta[limit].ChangeSeq
	truncated := delta[:limit]
	for len(truncated) > 0 && truncated[len(truncated)-1].ChangeSeq == overflowSeq {
		truncated = truncated[:len(truncated)-1]
	}

	if len(truncated) == 0 {
		// A single push is larger than the page the client asked for. Returning it whole is the
		// only answer that lets the client advance at all, which makes `limit` a request rather
		// than a cap. It is bounded by the push limit, not by the client's number.
		wholeSequence, _, err := store.SelectDelta(ctx, pool, accountID, overflowSeq-1, overflowSeq, MaxChangesPerBatch+1)
		if err != nil {
			return PullResult{}, err
		}
		if len(wholeSequence) > MaxChangesPerBatch {
			// Unreachable while pushes are capped at MaxChangesPerBatch. Failing here is the
			// alternative to returning part of a sequence value and calling it complete.
			return PullResult{}, fmt.Errorf("change sequence %d holds more than %d entities", overflowSeq, MaxChangesPerBatch)
		}
		return PullResult{
			Changes: changesOf(wholeSequence),
			Cursor:  overflowSeq,
			HasMore: overflowSeq < upperBound,
		}, nil
	}

	cursor := truncated[len(truncated)-1].ChangeSeq
	return PullResult{Changes: changesOf(truncated), Cursor: cursor, HasMore: cursor < upperBound}, nil
}

func changesOf(delta []store.DeltaRow) []json.RawMessage {
	changes := make([]json.RawMessage, 0, len(delta))
	for _, row := range delta {
		changes = append(changes, row.Change)
	}
	return changes
}
