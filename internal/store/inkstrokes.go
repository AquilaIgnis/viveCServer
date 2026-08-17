package store

import (
	"errors"
	"fmt"
)

// maxInkPointsBytes caps one stroke's, mask's or lasso's encoded points.
//
// NotebookTransferManager.MAX_POINT_BYTES, so a page that imports from a `.vive` bundle also syncs.
// The 4 MB push cap (syncPlan.md §7) binds long before this in practice — a row this large could
// not share a batch with anything, and base64 costs a third on top — but a limit that disagreed with
// the bundle format would mean a notebook the app can carry and this server refuses.
const maxInkPointsBytes = 4 * 1024 * 1024

// InkStrokeFields is one stroke, matching InkStrokeEntity in the app.
//
// Immutable in everything that draws it. What can change after the first write is small and takes
// the ordinary OCC path with every other kind: [DeletedAt] on the envelope, which an erase sets and
// an undo clears again, plus [ColorARGB]/[ColorFollowsTheme] and [GroupID].
type InkStrokeFields struct {
	PageID string `json:"pageId"`

	// DrawOrder is the app's `ink_strokes.seq`, and a page-scoped Lamport clock rather than a count:
	// two devices drawing offline on one page allocate the same value on purpose, and the tie is
	// broken by id. Nothing here may assume it is unique or dense
	// (viveNotes/memory/inkSyncPlan.md §1).
	//
	// **Not** named `seq` on the wire, though that is its column name in both databases: `seq` is a
	// reserved envelope key holding the account's change sequence. A stroke that sent its draw order
	// under that name would have it stripped as an envelope field and stored nowhere — the ink would
	// arrive and paint in the wrong order, with nothing failing.
	DrawOrder int32 `json:"drawOrder"`

	BrushFamily  string  `json:"brushFamily"`
	BrushVersion int32   `json:"brushVersion"`
	SizeDp       float32 `json:"sizeDp"`
	ColorARGB    int32   `json:"colorArgb"`

	// ColorFollowsTheme is a tri-state: true, false, or absent on a stroke drawn before the app
	// recorded whether its colour was the automatic one. A pointer keeps the third state, which a
	// bool would silently turn into "deliberate".
	ColorFollowsTheme *bool `json:"colorFollowsTheme"`

	Epsilon       float32 `json:"epsilon"`
	Stabilization int32   `json:"stabilization"`

	MinX float32 `json:"minX"`
	MinY float32 `json:"minY"`
	MaxX float32 `json:"maxX"`
	MaxY float32 `json:"maxY"`

	// Points is the app's `ink/v1` blob, carried as base64 by encoding/json and never decoded here.
	Points []byte `json:"points"`

	// Enc is the encoder that wrote Points, per row, so a format change is a rolling change rather
	// than a resync (`InkStrokeEntity.enc`).
	Enc string `json:"enc"`

	CreatedAt int64 `json:"createdAt"`

	// GroupID is an optional logical object group. Absent for a stroke in no group.
	GroupID *string `json:"groupId"`
}

const inkStrokeChangeJSON = `extra || jsonb_build_object(
		'kind', 'inkStroke',
		'id', id,
		'version', version,
		'seq', change_seq,
		'deletedAt', deleted_at,
		'updatedAt', updated_at,
		'pageId', page_id,
		'drawOrder', seq,
		'brushFamily', brush_family,
		'brushVersion', brush_version,
		'sizeDp', size_dp,
		'colorArgb', color_argb,
		'colorFollowsTheme', color_follows_theme,
		'epsilon', epsilon,
		'stabilization', stabilization,
		'minX', min_x,
		'minY', min_y,
		'maxX', max_x,
		'maxY', max_y,
		'points', translate(encode(points, 'base64'), E'\n', ''),
		'enc', enc,
		'createdAt', created_at,
		'groupId', group_id)`

func (fields *InkStrokeFields) ParentID() string {
	return fields.PageID
}

func (fields *InkStrokeFields) Validate() error {
	if err := ValidateEntityID(fields.PageID); err != nil {
		return err
	}
	if fields.DrawOrder < 0 {
		// The app's own invariant (`StarterInkPageFixture`), and the one thing about `seq` this
		// server is entitled to know: it is allocated upwards from zero.
		return errors.New("drawOrder must not be negative")
	}
	if fields.BrushFamily == "" {
		return errors.New("brushFamily must not be empty")
	}
	if err := validateSyncText("brushFamily", fields.BrushFamily, maxCodecIDChars); err != nil {
		return err
	}
	if err := validateInkPoints(fields.Points); err != nil {
		return err
	}
	if err := validateSyncText("enc", fields.Enc, maxCodecIDChars); err != nil {
		return err
	}
	if fields.GroupID != nil {
		if err := ValidateEntityID(*fields.GroupID); err != nil {
			return fmt.Errorf("groupId: %w", err)
		}
	}
	return nil
}

func (fields *InkStrokeFields) upsertStatement(envelope ChangeEnvelope, baseVersion int64) (string, []any) {
	const statement = `
		INSERT INTO ink_strokes AS stored (
			account_id, id, version, change_seq, deleted_at, updated_at, server_updated_at,
			last_writer, extra,
			page_id, seq, brush_family, brush_version, size_dp, color_argb, color_follows_theme,
			epsilon, stabilization, min_x, min_y, max_x, max_y, points, enc, created_at, group_id
		)
		VALUES ($1::uuid, $2, $3, $4, $5, $6, now(), $7::uuid, $8::jsonb,
			$9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24, $25)
		ON CONFLICT (account_id, id) DO UPDATE SET
			version = EXCLUDED.version,
			change_seq = EXCLUDED.change_seq,
			deleted_at = EXCLUDED.deleted_at,
			updated_at = EXCLUDED.updated_at,
			server_updated_at = EXCLUDED.server_updated_at,
			last_writer = EXCLUDED.last_writer,
			extra = EXCLUDED.extra,
			page_id = EXCLUDED.page_id,
			seq = EXCLUDED.seq,
			brush_family = EXCLUDED.brush_family,
			brush_version = EXCLUDED.brush_version,
			size_dp = EXCLUDED.size_dp,
			color_argb = EXCLUDED.color_argb,
			color_follows_theme = EXCLUDED.color_follows_theme,
			epsilon = EXCLUDED.epsilon,
			stabilization = EXCLUDED.stabilization,
			min_x = EXCLUDED.min_x,
			min_y = EXCLUDED.min_y,
			max_x = EXCLUDED.max_x,
			max_y = EXCLUDED.max_y,
			points = EXCLUDED.points,
			enc = EXCLUDED.enc,
			created_at = EXCLUDED.created_at,
			group_id = EXCLUDED.group_id
		WHERE stored.version = $26`

	return statement, []any{
		envelope.AccountID, envelope.ID, envelope.Version, envelope.ChangeSeq, envelope.DeletedAt,
		envelope.UpdatedAt, envelope.LastWriter, envelope.Extra,
		fields.PageID, fields.DrawOrder, fields.BrushFamily, fields.BrushVersion, fields.SizeDp,
		fields.ColorARGB, fields.ColorFollowsTheme, fields.Epsilon, fields.Stabilization,
		fields.MinX, fields.MinY, fields.MaxX, fields.MaxY, fields.Points,
		inkEncodingOr(fields.Enc), fields.CreatedAt, fields.GroupID,
		baseVersion,
	}
}

// validateInkPoints is the one check every ink kind makes on its payload.
//
// Empty is allowed rather than rejected: what an encoder writes for a degenerate stroke is the
// app's business, and a row this server refused would sit in an outbox retrying for ever.
func validateInkPoints(points []byte) error {
	if len(points) > maxInkPointsBytes {
		return fmt.Errorf("%w: points is larger than %d bytes", ErrTooLarge, maxInkPointsBytes)
	}
	return nil
}

// inkEncodingOr defaults the encoder id the way `defaultEncoding` does for documents: a client that
// says nothing means the format the app has always written.
func inkEncodingOr(encoding string) string {
	if encoding == "" {
		return defaultInkEncoding
	}
	return encoding
}

// defaultInkEncoding is `InkCodec`'s only encoder id, and the column default in migration 00005.
const defaultInkEncoding = "ink/v1"
