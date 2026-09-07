package store

import "encoding/json"

// NotebookFields is the notebook-specific half of a synced change.
//
// The JSON names match NotebookEntity in the app
// (app/src/main/java/com/vivenotes/data/db/Entities.kt) field for field, because the app pushes its
// row as it stands rather than translating it first. `expanded` remains on the wire for contract
// compatibility, but current clients preserve their device-local rail state when applying rows and
// do not queue expansion-only writes.
//
// `closedAt` and `cloudOnlyAt` are the shelf a notebook sits on. They are account-wide by the app's
// own decision — the flag travels on the entity, so it goes where the entity goes — and this server
// stores them in columns of their own because it acts on them: see [NotebookFields.RetainOnDelete].
type NotebookFields struct {
	Name      string `json:"name"`
	ColorArgb int32  `json:"colorArgb"`
	SortIndex int32  `json:"sortIndex"`
	Expanded  bool   `json:"expanded"`

	// ClosedAt is the client wall clock at which the notebook left the rail, or nil while it is
	// open. A pointer rather than a zero value because "not closed" and "closed at the epoch" are
	// different rows, and only one of them is a state the app can produce.
	ClosedAt *int64 `json:"closedAt"`

	// CloudOnlyAt is the client wall clock from which this server holds the only copy of the
	// contents, or nil while a device still has them. Always set together with ClosedAt: a
	// notebook whose bytes are not on the device cannot be in the rail.
	CloudOnlyAt *int64 `json:"cloudOnlyAt"`

	CreatedAt int64 `json:"createdAt"`
}

// notebookChangeJSON renders a notebook row as the object a client receives.
//
// `extra ||` comes first so that the keys built here win: an unrecognised field stored by an older
// server can never come back claiming to be the row's `version` or `id`.
const notebookChangeJSON = `extra || jsonb_build_object(
		'kind', 'notebook',
		'id', id,
		'version', version,
		'seq', change_seq,
		'deletedAt', deleted_at,
		'updatedAt', updated_at,
		'name', name,
		'colorArgb', color_argb,
		'sortIndex', sort_index,
		'expanded', expanded,
		'closedAt', closed_at,
		'cloudOnlyAt', cloud_only_at,
		'createdAt', created_at)`

// ParentID is empty: a notebook is the root of the hierarchy.
func (fields *NotebookFields) ParentID() string {
	return ""
}

func (fields *NotebookFields) Validate() error {
	return validateSyncText("name", fields.Name, maxNameChars)
}

// RetainOnDelete turns a closed notebook's tombstone into cloud retention, and reports that it did.
//
// A closed notebook is one the account has put on a shelf rather than one it is using, and moving
// it to the cloud leaves this server holding the only copy of its contents. Storing a tombstone for
// it would therefore accept a delete from a device that does not have the bytes any more — and the
// device asking is, by construction, the one that cannot check what it is throwing away. So the row
// stays live and gains `cloudOnlyAt` instead: the notebook leaves the device that asked, arrives on
// every device's "in the cloud" shelf on the next pull, and can be downloaded again from any of
// them. Deleting a notebook that is *open* means exactly what it says and is untouched.
//
// `stored` is the row as a pull would render it, which the push path has already read for the
// version check. It is the fallback for both flags because a client may push a delete for a
// notebook it closed in an earlier batch, in which case its own change carries neither.
//
// Two rows are never retained, and both for the same reason — there is nothing hosted to keep:
// one this account has no row for at all (a client that created, closed and deleted a notebook
// before it ever synced), and one that is already a tombstone. Retaining either would not preserve
// a notebook, it would conjure an empty one onto every device's shelf.
func (fields *NotebookFields) RetainOnDelete(stored json.RawMessage, deletedAt int64) bool {
	storedShelf, hosted := notebookShelfOf(stored)
	if !hosted {
		return false
	}

	if fields.ClosedAt == nil {
		fields.ClosedAt = storedShelf.ClosedAt
	}
	if fields.ClosedAt == nil {
		return false
	}

	if fields.CloudOnlyAt == nil {
		fields.CloudOnlyAt = storedShelf.CloudOnlyAt
	}
	if fields.CloudOnlyAt == nil {
		// The moment the device stopped holding it, in the clock the rest of the row is written in.
		// Reusing the delete's own timestamp rather than reading the server clock keeps the two
		// shelf columns comparable with `updatedAt`, which is what the app displays.
		fields.CloudOnlyAt = &deletedAt
	}
	return true
}

// notebookShelf is the part of a stored notebook that decides what its deletion means.
type notebookShelf struct {
	ClosedAt    *int64 `json:"closedAt"`
	CloudOnlyAt *int64 `json:"cloudOnlyAt"`
	DeletedAt   *int64 `json:"deletedAt"`
}

// notebookShelfOf reads that out of a rendered notebook change, reporting whether the account holds
// a live notebook at all.
//
// Reading it back out of JSON rather than selecting the columns again is what keeps the push path
// at its three queries per kind (syncengine.readBatchContext). A row rendered by this build always
// carries all three keys, so a missing or unreadable one means the value is genuinely absent.
func notebookShelfOf(stored json.RawMessage) (shelf notebookShelf, hosted bool) {
	if len(stored) == 0 {
		return notebookShelf{}, false
	}
	if err := json.Unmarshal(stored, &shelf); err != nil {
		return notebookShelf{}, false
	}
	return shelf, shelf.DeletedAt == nil
}

func (fields *NotebookFields) upsertStatement(envelope ChangeEnvelope, baseVersion int64) (string, []any) {
	const statement = `
		INSERT INTO notebooks AS stored (
			account_id, id, version, change_seq, deleted_at, updated_at, server_updated_at,
			last_writer, extra,
			name, color_argb, sort_index, expanded, closed_at, cloud_only_at, created_at
		)
		VALUES ($1::uuid, $2, $3, $4, $5, $6, now(), $7::uuid, $8::jsonb, $9, $10, $11, $12, $13, $14, $15)
		ON CONFLICT (account_id, id) DO UPDATE SET
			version = EXCLUDED.version,
			change_seq = EXCLUDED.change_seq,
			deleted_at = EXCLUDED.deleted_at,
			updated_at = EXCLUDED.updated_at,
			server_updated_at = EXCLUDED.server_updated_at,
			last_writer = EXCLUDED.last_writer,
			extra = EXCLUDED.extra,
			name = EXCLUDED.name,
			color_argb = EXCLUDED.color_argb,
			sort_index = EXCLUDED.sort_index,
			expanded = EXCLUDED.expanded,
			closed_at = EXCLUDED.closed_at,
			cloud_only_at = EXCLUDED.cloud_only_at,
			created_at = EXCLUDED.created_at
		WHERE stored.version = $16`

	return statement, []any{
		envelope.AccountID, envelope.ID, envelope.Version, envelope.ChangeSeq, envelope.DeletedAt,
		envelope.UpdatedAt, envelope.LastWriter, envelope.Extra,
		fields.Name, fields.ColorArgb, fields.SortIndex, fields.Expanded,
		fields.ClosedAt, fields.CloudOnlyAt, fields.CreatedAt,
		baseVersion,
	}
}
