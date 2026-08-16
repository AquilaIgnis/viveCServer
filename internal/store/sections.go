package store

// SectionFields is the section-specific half of a synced change, matching SectionEntity in the app.
type SectionFields struct {
	NotebookID string `json:"notebookId"`
	Name       string `json:"name"`
	ColorArgb  int32  `json:"colorArgb"`
	SortIndex  int32  `json:"sortIndex"`
	CreatedAt  int64  `json:"createdAt"`
}

const sectionChangeJSON = `extra || jsonb_build_object(
		'kind', 'section',
		'id', id,
		'version', version,
		'seq', change_seq,
		'deletedAt', deleted_at,
		'updatedAt', updated_at,
		'notebookId', notebook_id,
		'name', name,
		'colorArgb', color_argb,
		'sortIndex', sort_index,
		'createdAt', created_at)`

func (fields *SectionFields) ParentID() string {
	return fields.NotebookID
}

func (fields *SectionFields) Validate() error {
	if err := ValidateEntityID(fields.NotebookID); err != nil {
		return err
	}
	return validateSyncText("name", fields.Name, maxNameChars)
}

func (fields *SectionFields) upsertStatement(envelope ChangeEnvelope, baseVersion int64) (string, []any) {
	const statement = `
		INSERT INTO sections AS stored (
			account_id, id, version, change_seq, deleted_at, updated_at, server_updated_at,
			last_writer, extra,
			notebook_id, name, color_argb, sort_index, created_at
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
			notebook_id = EXCLUDED.notebook_id,
			name = EXCLUDED.name,
			color_argb = EXCLUDED.color_argb,
			sort_index = EXCLUDED.sort_index,
			created_at = EXCLUDED.created_at
		WHERE stored.version = $14`

	return statement, []any{
		envelope.AccountID, envelope.ID, envelope.Version, envelope.ChangeSeq, envelope.DeletedAt,
		envelope.UpdatedAt, envelope.LastWriter, envelope.Extra,
		fields.NotebookID, fields.Name, fields.ColorArgb, fields.SortIndex, fields.CreatedAt,
		baseVersion,
	}
}
