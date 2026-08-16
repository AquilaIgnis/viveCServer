package store

// NotebookFields is the notebook-specific half of a synced change.
//
// The JSON names match NotebookEntity in the app
// (app/src/main/java/com/vivenotes/data/db/Entities.kt) field for field, because the app pushes its
// row as it stands rather than translating it first. `expanded` is arguably per-device rail state
// and belongs in DataStore by the app's own rule, but it lives on the entity, so it syncs for now
// (syncPlan.md §12.2).
type NotebookFields struct {
	Name      string `json:"name"`
	ColorArgb int32  `json:"colorArgb"`
	SortIndex int32  `json:"sortIndex"`
	Expanded  bool   `json:"expanded"`
	CreatedAt int64  `json:"createdAt"`
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
		'createdAt', created_at)`

// ParentID is empty: a notebook is the root of the hierarchy.
func (fields *NotebookFields) ParentID() string {
	return ""
}

func (fields *NotebookFields) Validate() error {
	return validateSyncText("name", fields.Name, maxNameChars)
}

func (fields *NotebookFields) upsertStatement(envelope ChangeEnvelope, baseVersion int64) (string, []any) {
	const statement = `
		INSERT INTO notebooks AS stored (
			account_id, id, version, change_seq, deleted_at, updated_at, server_updated_at,
			last_writer, extra,
			name, color_argb, sort_index, expanded, created_at
		)
		VALUES ($1::uuid, $2, $3, $4, $5, $6, now(), $7::uuid, $8::jsonb, $9, $10, $11, $12, $13)
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
			created_at = EXCLUDED.created_at
		WHERE stored.version = $14`

	return statement, []any{
		envelope.AccountID, envelope.ID, envelope.Version, envelope.ChangeSeq, envelope.DeletedAt,
		envelope.UpdatedAt, envelope.LastWriter, envelope.Extra,
		fields.Name, fields.ColorArgb, fields.SortIndex, fields.Expanded, fields.CreatedAt,
		baseVersion,
	}
}
