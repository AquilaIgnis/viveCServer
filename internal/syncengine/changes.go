// Package syncengine turns a batch of client changes into stored rows, and stored rows back into
// the delta a client pulls.
//
// It is the only place that knows what a push means. It does not know what a note means: every
// field it handles is either an identifier, a version, or an opaque value it stores and returns
// unchanged (syncPlan.md §2).
package syncengine

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/AquilaIgnis/viveCServer/internal/store"
)

// Rejection reasons are a closed set, so a client can branch on them without reading prose. Each
// one has to carry enough for the client to act without a second round trip.
const (
	// ReasonVersionConflict means the entity moved on the server since the client last saw it. The
	// current server state travels with the rejection.
	ReasonVersionConflict = "version_conflict"

	// ReasonMissingParent means the notebook or section this entity hangs from has not been
	// uploaded. The client retries in the next batch once the parent syncs.
	ReasonMissingParent = "missing_parent"

	// ReasonTooLarge means a field exceeded the limits the app's own notebook bundles enforce.
	ReasonTooLarge = "too_large"

	// ReasonMalformed means the change could not be read as the kind it claims to be.
	ReasonMalformed = "malformed"

	// ReasonMissingBlob is not produced yet. It arrives with page content and attachments in S3/S5,
	// and is named here so the closed set is visible in one place.
)

// MaxChangesPerBatch caps one push. The client paginates; the body limit in httpapi caps the other
// half of syncPlan.md §7's "512 entities or 4 MB, whichever comes first".
const MaxChangesPerBatch = 512

// maxExtraBytes caps the unrecognised fields stored for one entity.
//
// Without it, forward compatibility doubles as an unmetered write API: anything a client sends
// under a name this build does not know is kept verbatim and returned to every device for ever.
const maxExtraBytes = 16 * 1024

// AppliedChange reports one entity that was written, and the version it now holds.
type AppliedChange struct {
	Kind    string `json:"kind"`
	ID      string `json:"id"`
	Version int64  `json:"version"`
}

// RejectedChange reports one entity that was not written, and why.
type RejectedChange struct {
	Kind   string `json:"kind"`
	ID     string `json:"id"`
	Reason string `json:"reason"`

	// Message is for a human reading a log. Client logic branches on Reason.
	Message string `json:"message,omitempty"`

	// Current is the stored row, rendered exactly as a pull would return it, so a client can rebase
	// on it directly. Absent when there is no stored row to describe.
	Current json.RawMessage `json:"current,omitempty"`
}

// decodedChange is one element of a push after it has been read but before it has been decided.
type decodedChange struct {
	kind        *store.SyncKind
	envelope    store.ChangeEnvelope
	fields      store.EntityFields
	baseVersion int64

	// rejection is set when the change was refused while being read, which is every reason that
	// needs no database access. Such a change keeps its place in the ordering and is reported like
	// any other rejection.
	rejection *RejectedChange
}

// decodeChanges reads every element of a push and sorts them so parents come before children.
//
// The ordering is what makes a batch self-contained: a section pushed in the same batch as its
// notebook is applied after it, so the parent check sees it. Sorting is stable, so two entities of
// one kind stay in the order the client sent them.
func decodeChanges(rawChanges []json.RawMessage) []decodedChange {
	decoded := make([]decodedChange, 0, len(rawChanges))
	seenEntities := make(map[string]struct{}, len(rawChanges))
	for _, raw := range rawChanges {
		decoded = append(decoded, decodeChange(raw, seenEntities))
	}

	sort.SliceStable(decoded, func(first, second int) bool {
		return applyRankOf(decoded[first]) < applyRankOf(decoded[second])
	})
	return decoded
}

// applyRankOf orders a change among the kinds. A change whose kind could not be resolved sorts
// last, where it cannot break up another kind's run.
func applyRankOf(change decodedChange) int {
	if change.kind == nil {
		return len(store.SyncKindsInApplyOrder())
	}
	return change.kind.Rank
}

func decodeChange(raw json.RawMessage, seenEntities map[string]struct{}) decodedChange {
	var envelope struct {
		Kind        string `json:"kind"`
		ID          string `json:"id"`
		BaseVersion int64  `json:"baseVersion"`
		DeletedAt   *int64 `json:"deletedAt"`
		UpdatedAt   int64  `json:"updatedAt"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return refuse(nil, "", "", ReasonMalformed, "change is not a readable object: "+err.Error())
	}

	kind, known := store.SyncKindByName(envelope.Kind)
	if !known {
		// An unknown kind is not necessarily a broken client: it is what a newer app looks like to
		// a server that has not been upgraded yet. There is no table to put it in either way, so it
		// is refused on its own and the rest of the batch still lands. The entity stays in the
		// client's outbox and succeeds after the upgrade.
		return refuse(nil, envelope.Kind, envelope.ID, ReasonMalformed, "unknown entity kind")
	}

	if err := store.ValidateEntityID(envelope.ID); err != nil {
		return refuse(kind, kind.Name, envelope.ID, reasonFor(err), err.Error())
	}
	if envelope.BaseVersion < 0 {
		return refuse(kind, kind.Name, envelope.ID, ReasonMalformed, "baseVersion must not be negative")
	}

	// One entity twice in one batch would write both copies against the same stored version, and
	// the second write would then find a version the first had already moved. The client's outbox
	// is keyed by entity and coalesces (SD6), so this is a client bug rather than a race.
	entityKey := kind.Name + "\x00" + envelope.ID
	if _, duplicated := seenEntities[entityKey]; duplicated {
		return refuse(kind, kind.Name, envelope.ID, ReasonMalformed, "the same entity appears more than once in this batch")
	}
	seenEntities[entityKey] = struct{}{}

	fields := kind.NewFields()
	if err := json.Unmarshal(raw, fields); err != nil {
		return refuse(kind, kind.Name, envelope.ID, ReasonMalformed, "change does not fit its kind: "+err.Error())
	}
	if err := fields.Validate(); err != nil {
		return refuse(kind, kind.Name, envelope.ID, reasonFor(err), err.Error())
	}

	extra, err := collectExtraFields(raw, kind)
	if err != nil {
		return refuse(kind, kind.Name, envelope.ID, reasonFor(err), err.Error())
	}

	return decodedChange{
		kind:        kind,
		baseVersion: envelope.BaseVersion,
		fields:      fields,
		envelope: store.ChangeEnvelope{
			ID:        envelope.ID,
			DeletedAt: envelope.DeletedAt,
			UpdatedAt: envelope.UpdatedAt,
			Extra:     extra,
		},
	}
}

// collectExtraFields keeps whatever this build does not recognise.
//
// Unknown keys are collected from the change itself rather than expected under an `extra` object,
// because the client that has them does not think of them as extra: a newer app ships a field as a
// first-class column and sends it at the top level. Anything else would mean the app could only
// stay forward-compatible by predicting which fields a future server would lack. Returned the same
// way on pull, so a value that arrived at the top level leaves at the top level.
func collectExtraFields(raw json.RawMessage, kind *store.SyncKind) (string, error) {
	var byKey map[string]json.RawMessage
	if err := json.Unmarshal(raw, &byKey); err != nil {
		return "", fmt.Errorf("change is not a readable object: %w", err)
	}

	for key := range byKey {
		if store.IsReservedChangeKey(key) || kind.ClaimsField(key) {
			delete(byKey, key)
		}
	}
	if len(byKey) == 0 {
		return "{}", nil
	}

	encoded, err := json.Marshal(byKey)
	if err != nil {
		return "", fmt.Errorf("unrecognised fields cannot be stored: %w", err)
	}
	if len(encoded) > maxExtraBytes {
		return "", fmt.Errorf("%w: unrecognised fields are larger than %d bytes", store.ErrTooLarge, maxExtraBytes)
	}
	return string(encoded), nil
}

// reasonFor maps a validation failure onto the closed set of rejection reasons.
func reasonFor(err error) string {
	if errors.Is(err, store.ErrTooLarge) {
		return ReasonTooLarge
	}
	return ReasonMalformed
}

func refuse(kind *store.SyncKind, kindName string, entityID string, reason string, message string) decodedChange {
	return decodedChange{
		kind: kind,
		envelope: store.ChangeEnvelope{
			ID: entityID,
		},
		rejection: &RejectedChange{
			Kind:    kindName,
			ID:      entityID,
			Reason:  reason,
			Message: message,
		},
	}
}
