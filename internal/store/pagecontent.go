package store

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
)

// maxDocBytes caps one document body.
//
// Not the app's own 16 MB ceiling (`DocumentRevisions.MAX_DOCUMENT_BYTES`), which bounds a
// compressed revision blob rather than a live document, and which no push could carry anyway: a
// batch is capped at 4 MB (syncPlan.md §7) and base64 costs a third on top of the bytes. Two
// mebibytes leaves room for a document to travel with other changes in the same batch, and is far
// above a realistic body — ink lives in its own rows and images are attachments, so what is here is
// text, structure and equations.
//
// A document past this is rejected `too_large` rather than truncated. Raising it means raising the
// batch cap with it, which is R1's binary-framing question and belongs to S4.
const maxDocBytes = 2 << 20

// maxCodecIDChars mirrors NotebookTransferManager.MAX_FORMAT_CHARS.
const maxCodecIDChars = 128

// defaultEncoding is what a client that does not speak of encodings means (§3, H4).
const defaultEncoding = "none/1"

// ErrDigestMismatch marks a body that does not hash to the digest its sender claimed.
var ErrDigestMismatch = errors.New("doc does not match docSha256")

// PageContentFields is the document half of a page, matching PageContentEntity in the app.
//
// The server never parses [Doc]. It is bytes with a codec id, a wrapping id and a checksum, which is
// what SD1 means by holding a document opaquely: conflicts here resolve on the client, by three-way
// merge against the last acknowledged body (SD3), and nothing on this side needs to know what an
// outline is.
//
// [Doc] and [DocSHA256] are `[]byte`, so `encoding/json` carries them as base64 in both directions
// with no conversion of ours. Base64 costs a third of the payload; SD5 accepted that for v1 and R1
// holds a binary framing in reserve for ink, which is where the volume actually is.
type PageContentFields struct {
	// PageID is the page this body belongs to. One body per page is a database constraint rather
	// than a convention here — see migration 00004.
	PageID string `json:"pageId"`

	Doc []byte `json:"doc"`

	// DocSHA256 is optional on a push and always present on a pull.
	//
	// When a client sends one it is checked, not trusted: the stored digest is always recomputed
	// from the bytes that arrived. That check is the cheap half of reliability here — a body
	// truncated in transit becomes a rejected entity instead of a document that decodes to nothing
	// on every device that pulls it.
	DocSHA256 []byte `json:"docSha256"`

	Format string `json:"format"`

	// Enc is how Doc is wrapped: `none/1` unless something wraps it. Empty means the same thing,
	// because a client that has never heard of encodings still writes plain bodies.
	Enc string `json:"enc"`
}

const pageContentChangeJSON = `extra || jsonb_build_object(
		'kind', 'pageContent',
		'id', id,
		'version', version,
		'seq', change_seq,
		'deletedAt', deleted_at,
		'updatedAt', updated_at,
		'pageId', page_id,
		'doc', translate(encode(doc, 'base64'), E'\n', ''),
		'docSha256', translate(encode(doc_sha256, 'base64'), E'\n', ''),
		'format', format,
		'enc', enc)`

func (fields *PageContentFields) ParentID() string {
	return fields.PageID
}

func (fields *PageContentFields) Validate() error {
	if err := ValidateEntityID(fields.PageID); err != nil {
		return err
	}
	if len(fields.Doc) > maxDocBytes {
		return fmt.Errorf("%w: doc is larger than %d bytes", ErrTooLarge, maxDocBytes)
	}
	// Empty is allowed: a tombstoned body is still a row, and the bytes it used to hold are not
	// worth carrying to say that it is gone.
	if fields.Format == "" {
		return errors.New("format must not be empty")
	}
	if err := validateSyncText("format", fields.Format, maxCodecIDChars); err != nil {
		return err
	}
	if err := validateSyncText("enc", fields.Enc, maxCodecIDChars); err != nil {
		return err
	}
	if len(fields.DocSHA256) > 0 {
		digest := sha256.Sum256(fields.Doc)
		if !bytes.Equal(fields.DocSHA256, digest[:]) {
			return ErrDigestMismatch
		}
	}
	return nil
}

func (fields *PageContentFields) upsertStatement(envelope ChangeEnvelope, baseVersion int64) (string, []any) {
	const statement = `
		INSERT INTO page_content AS stored (
			account_id, id, version, change_seq, deleted_at, updated_at, server_updated_at,
			last_writer, extra,
			page_id, doc, doc_sha256, format, enc
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
			page_id = EXCLUDED.page_id,
			doc = EXCLUDED.doc,
			doc_sha256 = EXCLUDED.doc_sha256,
			format = EXCLUDED.format,
			enc = EXCLUDED.enc
		WHERE stored.version = $14`

	// Computed rather than taken from the wire even when the client sent one it agreed with.
	// Validate has already rejected a disagreement, and hashing here means the column is a property
	// of the stored bytes rather than of a claim about them.
	digest := sha256.Sum256(fields.Doc)

	encoding := fields.Enc
	if encoding == "" {
		encoding = defaultEncoding
	}

	return statement, []any{
		envelope.AccountID, envelope.ID, envelope.Version, envelope.ChangeSeq, envelope.DeletedAt,
		envelope.UpdatedAt, envelope.LastWriter, envelope.Extra,
		fields.PageID, fields.Doc, digest[:], fields.Format, encoding,
		baseVersion,
	}
}
