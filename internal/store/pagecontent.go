package store

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"

	"github.com/AquilaIgnis/viveCServer/internal/blob"
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
// batch cap with it, which is R1's binary-framing question. S4 answered it for now: gzip on the wire
// recovers most of what base64 costs a document, and a binary framing for ink batches stays in
// reserve rather than becoming a second encoding for the whole protocol (syncPlan.md §13.8).
const maxDocBytes = 2 << 20

// maxBlobRefsPerPage caps how many attachments one document may name.
//
// Generous against any real page — the app re-encodes every import and a page is a screen, not an
// album — and low enough that the reference set of one document stays a small write. Its purpose
// is the same as maxExtraBytes': a field the client fills in must not be an unmetered write API.
const maxBlobRefsPerPage = 512

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
	// than a convention here — see `page_content`'s UNIQUE (account_id, page_id).
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

	// BlobRefs are the attachment digests this document references — SD7, and the only way the
	// server can ever know.
	//
	// The document is opaque here (§2), so reachability cannot be discovered from it: the client
	// extracts the ids while pushing, and the server refuses the push if it does not already hold
	// every one of them. That refusal is what enforces `plan.md` Phase 8's ordering requirement —
	// attachments upload before the change referencing them — rather than trusting a client to
	// keep to it. The server can never hold a dangling reference because it will not accept one.
	//
	// Absent and empty mean the same thing, which matters for forward compatibility: a client built
	// before this field existed pushes documents with no `blobRefs` key, and a document with no
	// pictures in it references nothing either way.
	BlobRefs []string `json:"blobRefs"`
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
		'enc', enc,
		'blobRefs', COALESCE((
			SELECT jsonb_agg(encode(reference.sha256, 'hex') ORDER BY reference.sha256)
			FROM page_blob_refs AS reference
			WHERE reference.account_id = page_content.account_id
			  AND reference.page_id = page_content.page_id
		), '[]'::jsonb))`

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
	if len(fields.BlobRefs) > maxBlobRefsPerPage {
		return fmt.Errorf("%w: a page references more than %d attachments", ErrTooLarge, maxBlobRefsPerPage)
	}
	for _, digest := range fields.BlobRefs {
		if err := blob.ValidateDigest(digest); err != nil {
			return fmt.Errorf("blobRefs: %w", err)
		}
	}
	return nil
}

// RequiredBlobDigests reports the attachments this document names, which the server must already
// hold for the push to be accepted (SD7).
func (fields *PageContentFields) RequiredBlobDigests(entityID string) ([]string, error) {
	return fields.BlobRefs, nil
}

// auxiliaryStatements keeps `page_blob_refs` in step with the document that produced it.
//
// A replace rather than a merge: the reference set of a document is whatever its current version
// says it is, so a picture deleted from a page has to stop being a reason to keep its bytes. The
// delete runs even when the insert has nothing to add, which is also how a tombstoned body releases
// what it used to hold.
//
// Both statements are queued into the same pipelined transaction as the row itself (see
// WriteChanges), so a document and its references can never be separately visible.
func (fields *PageContentFields) auxiliaryStatements(envelope ChangeEnvelope) []auxiliaryStatement {
	statements := []auxiliaryStatement{{
		sql:       `DELETE FROM page_blob_refs WHERE account_id = $1::uuid AND page_id = $2`,
		arguments: []any{envelope.AccountID, fields.PageID},
	}}

	// A tombstoned body references nothing, whatever it still carries. Keeping its references would
	// hold the bytes of a picture on a deleted page for as long as the tombstone survives.
	if envelope.DeletedAt != nil || len(fields.BlobRefs) == 0 {
		return statements
	}

	digests := make([][]byte, 0, len(fields.BlobRefs))
	for _, reference := range fields.BlobRefs {
		raw, err := blob.DecodeDigest(reference)
		if err != nil {
			// Unreachable: Validate has already refused a reference that is not a digest.
			continue
		}
		digests = append(digests, raw)
	}

	statements = append(statements, auxiliaryStatement{
		// One statement for the whole set rather than one per digest: a page with twenty pictures
		// is one round trip in the pipeline instead of twenty.
		sql: `INSERT INTO page_blob_refs (account_id, page_id, sha256)
			SELECT $1::uuid, $2, digest FROM unnest($3::bytea[]) AS digest
			ON CONFLICT DO NOTHING`,
		arguments: []any{envelope.AccountID, fields.PageID, digests},
	})
	return statements
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
