package store

import (
	"errors"
	"fmt"

	"github.com/AquilaIgnis/viveCServer/internal/blob"
)

// maxMimeTypeChars bounds the media type an attachment declares. RFC 6838 caps a registered type
// and subtree at 127 characters each; this is that, doubled, and it is a sanity bound rather than a
// grammar check — the server never dispatches on this value, it stores it and hands it back.
const maxMimeTypeChars = 255

// AttachmentFields is what is known about a picture, matching AttachmentEntity in the app.
//
// **The entity's id is the attachment's digest**, because that is what the app's own primary key
// is — the lowercase hex SHA-256 of the stored bytes. So this kind has no `sha256` field: adding
// one would create a second place for an identity to be written and a first opportunity for the two
// to disagree. [RequiredBlobDigests] is where the id is read as the content address it is.
//
// The pixels are not here and never travel through the change protocol. They are uploaded to
// `/v1/blobs/{sha256}` before this row is pushed, and a push whose bytes are absent is rejected
// `missing_blob` (SD7). That ordering is the whole of the invariant: a device that pulls this row
// can always fetch what it describes.
type AttachmentFields struct {
	MimeType string `json:"mimeType"`

	// What was stored, after the app's import re-encode. Layout metadata: the server does not
	// decode the picture and has no way to check these, and nothing it enforces depends on them.
	PixelWidth  int32 `json:"pixelWidth"`
	PixelHeight int32 `json:"pixelHeight"`

	// The size the client believes the bytes are. Checked against the stored blob on every push —
	// see [ErrAttachmentSizeMismatch] — because a row that disagrees with the bytes it names is
	// the same class of quiet corruption as a document that disagrees with its digest.
	ByteCount int64 `json:"byteCount"`

	CreatedAt int64 `json:"createdAt"`
}

// ErrAttachmentSizeMismatch marks an attachment whose declared size is not the size of the blob its
// id names.
var ErrAttachmentSizeMismatch = errors.New("byteCount does not match the stored blob")

const attachmentChangeJSON = `extra || jsonb_build_object(
		'kind', 'attachment',
		'id', id,
		'version', version,
		'seq', change_seq,
		'deletedAt', deleted_at,
		'updatedAt', updated_at,
		'mimeType', mime_type,
		'pixelWidth', pixel_width,
		'pixelHeight', pixel_height,
		'byteCount', byte_count,
		'createdAt', created_at)`

// ParentID is empty: one picture can appear on several pages, so there is no page that owns it.
// The app's own entity says the same thing — "Not tied to a page by a foreign key, deliberately."
func (fields *AttachmentFields) ParentID() string { return "" }

func (fields *AttachmentFields) Validate() error {
	if fields.MimeType == "" {
		return errors.New("mimeType must not be empty")
	}
	if err := validateSyncText("mimeType", fields.MimeType, maxMimeTypeChars); err != nil {
		return err
	}
	if fields.PixelWidth < 0 || fields.PixelHeight < 0 {
		return errors.New("pixelWidth and pixelHeight must not be negative")
	}
	if fields.ByteCount < 0 {
		return errors.New("byteCount must not be negative")
	}
	return nil
}

// RequiredBlobDigests reports that an attachment needs the bytes its own id names.
//
// An id that is not a digest is refused here rather than by a CHECK constraint on the column. A
// constraint would abort the whole transaction, turning one client's malformed row into a batch
// that can never succeed and that the client retries for ever (§13.3); refusing it in Go rejects
// that one entity and lets the rest of the push land.
func (fields *AttachmentFields) RequiredBlobDigests(entityID string) ([]string, error) {
	if err := blob.ValidateDigest(entityID); err != nil {
		return nil, fmt.Errorf("an attachment id is the hex SHA-256 of its bytes: %w", err)
	}
	return []string{entityID}, nil
}

// CheckBlobPresence rejects an attachment whose declared size is not the size of the stored blob.
func (fields *AttachmentFields) CheckBlobPresence(entityID string, present map[string]BlobPresence) error {
	stored, held := present[entityID]
	if !held {
		return nil // The engine has already reported the absence as missing_blob.
	}
	if fields.ByteCount != stored.ByteCount {
		return fmt.Errorf("%w: the blob holds %d bytes, not %d",
			ErrAttachmentSizeMismatch, stored.ByteCount, fields.ByteCount)
	}
	return nil
}

func (fields *AttachmentFields) upsertStatement(envelope ChangeEnvelope, baseVersion int64) (string, []any) {
	const statement = `
		INSERT INTO attachments AS stored (
			account_id, id, version, change_seq, deleted_at, updated_at, server_updated_at,
			last_writer, extra,
			mime_type, pixel_width, pixel_height, byte_count, created_at
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
			mime_type = EXCLUDED.mime_type,
			pixel_width = EXCLUDED.pixel_width,
			pixel_height = EXCLUDED.pixel_height,
			byte_count = EXCLUDED.byte_count,
			created_at = EXCLUDED.created_at
		WHERE stored.version = $14`

	return statement, []any{
		envelope.AccountID, envelope.ID, envelope.Version, envelope.ChangeSeq, envelope.DeletedAt,
		envelope.UpdatedAt, envelope.LastWriter, envelope.Extra,
		fields.MimeType, fields.PixelWidth, fields.PixelHeight, fields.ByteCount, fields.CreatedAt,
		baseVersion,
	}
}
