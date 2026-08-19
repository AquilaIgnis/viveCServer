package store

// PageFields is the page-specific half of a synced change, matching PageEntity in the app.
//
// The document body is not here. It lives in `page_content`, which is a separate synced kind of its
// own, exactly as the app splits it so that listing a section's pages never loads every document.
type PageFields struct {
	SectionID string `json:"sectionId"`
	Title     string `json:"title"`
	SortIndex int32  `json:"sortIndex"`
	Preview   string `json:"preview"`
	CreatedAt int64  `json:"createdAt"`
}

const pageChangeJSON = `extra || jsonb_build_object(
		'kind', 'page',
		'id', id,
		'version', version,
		'seq', change_seq,
		'deletedAt', deleted_at,
		'updatedAt', updated_at,
		'sectionId', section_id,
		'title', title,
		'sortIndex', sort_index,
		'preview', preview,
		'createdAt', created_at)`

func (fields *PageFields) ParentID() string {
	return fields.SectionID
}

func (fields *PageFields) Validate() error {
	if err := ValidateEntityID(fields.SectionID); err != nil {
		return err
	}
	if err := validateSyncText("title", fields.Title, maxNameChars); err != nil {
		return err
	}
	return validateSyncText("preview", fields.Preview, maxPreviewChars)
}

func (fields *PageFields) upsertStatement(envelope ChangeEnvelope, baseVersion int64) (string, []any) {
	const statement = `
		INSERT INTO pages AS stored (
			account_id, id, version, change_seq, deleted_at, updated_at, server_updated_at,
			last_writer, extra,
			section_id, title, sort_index, preview, created_at
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
			section_id = EXCLUDED.section_id,
			title = EXCLUDED.title,
			sort_index = EXCLUDED.sort_index,
			preview = EXCLUDED.preview,
			created_at = EXCLUDED.created_at
		WHERE stored.version = $14`

	return statement, []any{
		envelope.AccountID, envelope.ID, envelope.Version, envelope.ChangeSeq, envelope.DeletedAt,
		envelope.UpdatedAt, envelope.LastWriter, envelope.Extra,
		fields.SectionID, fields.Title, fields.SortIndex, fields.Preview, fields.CreatedAt,
		baseVersion,
	}
}
