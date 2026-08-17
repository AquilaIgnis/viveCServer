package store

import (
	"errors"
	"fmt"
)

// Erases and moves share a file because they are the same thing twice: an immutable operation on a
// page, replayed by the client over the strokes it names. Their target handling in particular must
// not drift apart, and the way to guarantee that is for both to call the same function rather than
// for two files to agree.

// maxInkTargetsPerOperation caps the ids one operation may name.
//
// A guard against an unbounded array rather than a product limit: a lasso may legitimately hold
// every stroke on a page, and NotebookTransferManager allows 2,000,000 target rows across a whole
// notebook. What actually bounds a real request is the 4 MB push cap — 100,000 ids is already more
// than three of those — so this exists to keep one malformed entity from making the server allocate
// before the transaction it would abort.
const maxInkTargetsPerOperation = 100_000

// InkEraseFields is one eraser gesture, matching InkEraseEntity in the app.
//
// The mask and its targets are one entity on purpose. The app's own comment on `addPartialErase`
// says why they can never be separated: "the mask without its targets would be unsafe: replaying it
// against every page stroke would also erase ink drawn later."
type InkEraseFields struct {
	PageID string `json:"pageId"`

	// Mode is `Normal` or `Object` today. Stored as text and never checked against a list, so a new
	// eraser mode in the app does not need a server deploy before it can be stored.
	Mode string `json:"mode"`

	SizeDp float32 `json:"sizeDp"`

	// Points is the mask's `ink/v1` blob, opaque here.
	Points []byte `json:"points"`

	Enc       string `json:"enc"`
	CreatedAt int64  `json:"createdAt"`

	// TargetIDs are the strokes this gesture was allowed to affect. Inert ids, not references: see
	// [validateInkTargets].
	TargetIDs []string `json:"targetIds"`
}

// InkMoveFields is one lasso transform, matching InkMoveEntity in the app.
//
// Stored as its polygon and affine parameters rather than applied to the strokes it moves, because
// those strokes are immutable: rewriting their points would invalidate every stored lasso path that
// references the originals, and would put the whole selection back on the wire.
type InkMoveFields struct {
	PageID string `json:"pageId"`

	DxDp float32 `json:"dxDp"`
	DyDp float32 `json:"dyDp"`

	// A plain move stores an identity scale. The app replays every move through a resize for that
	// reason, and proves the identity case is a no-op rather than skipping it by type.
	ScaleX  float32 `json:"scaleX"`
	ScaleY  float32 `json:"scaleY"`
	AnchorX float32 `json:"anchorX"`
	AnchorY float32 `json:"anchorY"`

	// Points is the closed lasso polygon in page coordinates, opaque here.
	Points []byte `json:"points"`

	Enc       string   `json:"enc"`
	CreatedAt int64    `json:"createdAt"`
	TargetIDs []string `json:"targetIds"`
}

const inkEraseChangeJSON = `extra || jsonb_build_object(
		'kind', 'inkErase',
		'id', id,
		'version', version,
		'seq', change_seq,
		'deletedAt', deleted_at,
		'updatedAt', updated_at,
		'pageId', page_id,
		'mode', mode,
		'sizeDp', size_dp,
		'points', translate(encode(points, 'base64'), E'\n', ''),
		'enc', enc,
		'createdAt', created_at,
		'targetIds', to_jsonb(target_ids))`

const inkMoveChangeJSON = `extra || jsonb_build_object(
		'kind', 'inkMove',
		'id', id,
		'version', version,
		'seq', change_seq,
		'deletedAt', deleted_at,
		'updatedAt', updated_at,
		'pageId', page_id,
		'dxDp', dx_dp,
		'dyDp', dy_dp,
		'scaleX', scale_x,
		'scaleY', scale_y,
		'anchorX', anchor_x,
		'anchorY', anchor_y,
		'points', translate(encode(points, 'base64'), E'\n', ''),
		'enc', enc,
		'createdAt', created_at,
		'targetIds', to_jsonb(target_ids))`

func (fields *InkEraseFields) ParentID() string {
	return fields.PageID
}

func (fields *InkEraseFields) Validate() error {
	if err := ValidateEntityID(fields.PageID); err != nil {
		return err
	}
	if fields.Mode == "" {
		return errors.New("mode must not be empty")
	}
	if err := validateSyncText("mode", fields.Mode, maxCodecIDChars); err != nil {
		return err
	}
	if err := validateInkPoints(fields.Points); err != nil {
		return err
	}
	if err := validateSyncText("enc", fields.Enc, maxCodecIDChars); err != nil {
		return err
	}
	return validateInkTargets(fields.TargetIDs)
}

func (fields *InkMoveFields) ParentID() string {
	return fields.PageID
}

func (fields *InkMoveFields) Validate() error {
	if err := ValidateEntityID(fields.PageID); err != nil {
		return err
	}
	if err := validateInkPoints(fields.Points); err != nil {
		return err
	}
	if err := validateSyncText("enc", fields.Enc, maxCodecIDChars); err != nil {
		return err
	}
	return validateInkTargets(fields.TargetIDs)
}

// validateInkTargets checks the ids an operation names, and is the whole of what this server knows
// about them.
//
// They are **not** foreign keys, here or in the app's own schema, and one that names no stored
// stroke is inert — the client replays an operation against the strokes it can find and ignores the
// rest. Enforcing them would wedge a client permanently: a target recoloured after the operation was
// made reaches it in a later delta page than the operation naming it, and a target the app's
// seven-day purge already removed never arrives at all. Either one becomes a row that cannot be
// inserted, a transaction that rolls back with the cursor uncommitted, and a device that re-pulls
// the same delta for ever.
func validateInkTargets(targetIDs []string) error {
	if len(targetIDs) > maxInkTargetsPerOperation {
		return fmt.Errorf("%w: an operation names more than %d targets",
			ErrTooLarge, maxInkTargetsPerOperation)
	}
	for _, targetID := range targetIDs {
		if err := ValidateEntityID(targetID); err != nil {
			return fmt.Errorf("targetIds: %w", err)
		}
	}
	return nil
}

// inkTargetsOrEmpty keeps a null array out of a NOT NULL column. An operation with no targets is a
// no-op the client would not normally write, but a row is better than an aborted batch.
func inkTargetsOrEmpty(targetIDs []string) []string {
	if targetIDs == nil {
		return []string{}
	}
	return targetIDs
}

func (fields *InkEraseFields) upsertStatement(envelope ChangeEnvelope, baseVersion int64) (string, []any) {
	const statement = `
		INSERT INTO ink_erases AS stored (
			account_id, id, version, change_seq, deleted_at, updated_at, server_updated_at,
			last_writer, extra,
			page_id, mode, size_dp, points, enc, created_at, target_ids
		)
		VALUES ($1::uuid, $2, $3, $4, $5, $6, now(), $7::uuid, $8::jsonb,
			$9, $10, $11, $12, $13, $14, $15)
		ON CONFLICT (account_id, id) DO UPDATE SET
			version = EXCLUDED.version,
			change_seq = EXCLUDED.change_seq,
			deleted_at = EXCLUDED.deleted_at,
			updated_at = EXCLUDED.updated_at,
			server_updated_at = EXCLUDED.server_updated_at,
			last_writer = EXCLUDED.last_writer,
			extra = EXCLUDED.extra,
			page_id = EXCLUDED.page_id,
			mode = EXCLUDED.mode,
			size_dp = EXCLUDED.size_dp,
			points = EXCLUDED.points,
			enc = EXCLUDED.enc,
			created_at = EXCLUDED.created_at,
			target_ids = EXCLUDED.target_ids
		WHERE stored.version = $16`

	return statement, []any{
		envelope.AccountID, envelope.ID, envelope.Version, envelope.ChangeSeq, envelope.DeletedAt,
		envelope.UpdatedAt, envelope.LastWriter, envelope.Extra,
		fields.PageID, fields.Mode, fields.SizeDp, fields.Points, inkEncodingOr(fields.Enc),
		fields.CreatedAt, inkTargetsOrEmpty(fields.TargetIDs),
		baseVersion,
	}
}

func (fields *InkMoveFields) upsertStatement(envelope ChangeEnvelope, baseVersion int64) (string, []any) {
	const statement = `
		INSERT INTO ink_moves AS stored (
			account_id, id, version, change_seq, deleted_at, updated_at, server_updated_at,
			last_writer, extra,
			page_id, dx_dp, dy_dp, scale_x, scale_y, anchor_x, anchor_y, points, enc, created_at,
			target_ids
		)
		VALUES ($1::uuid, $2, $3, $4, $5, $6, now(), $7::uuid, $8::jsonb,
			$9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19)
		ON CONFLICT (account_id, id) DO UPDATE SET
			version = EXCLUDED.version,
			change_seq = EXCLUDED.change_seq,
			deleted_at = EXCLUDED.deleted_at,
			updated_at = EXCLUDED.updated_at,
			server_updated_at = EXCLUDED.server_updated_at,
			last_writer = EXCLUDED.last_writer,
			extra = EXCLUDED.extra,
			page_id = EXCLUDED.page_id,
			dx_dp = EXCLUDED.dx_dp,
			dy_dp = EXCLUDED.dy_dp,
			scale_x = EXCLUDED.scale_x,
			scale_y = EXCLUDED.scale_y,
			anchor_x = EXCLUDED.anchor_x,
			anchor_y = EXCLUDED.anchor_y,
			points = EXCLUDED.points,
			enc = EXCLUDED.enc,
			created_at = EXCLUDED.created_at,
			target_ids = EXCLUDED.target_ids
		WHERE stored.version = $20`

	return statement, []any{
		envelope.AccountID, envelope.ID, envelope.Version, envelope.ChangeSeq, envelope.DeletedAt,
		envelope.UpdatedAt, envelope.LastWriter, envelope.Extra,
		fields.PageID, fields.DxDp, fields.DyDp, fields.ScaleX, fields.ScaleY,
		fields.AnchorX, fields.AnchorY, fields.Points, inkEncodingOr(fields.Enc),
		fields.CreatedAt, inkTargetsOrEmpty(fields.TargetIDs),
		baseVersion,
	}
}
