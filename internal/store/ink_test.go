package store

import (
	"errors"
	"strings"
	"testing"
)

// TestNoKindClaimsAnEnvelopeKey guards the mistake ink very nearly shipped with.
//
// `ink_strokes.seq` is the column name in both databases, so the obvious JSON tag for it is `seq` —
// which is the envelope's account sequence. A kind that claimed it would render its own value last
// in `jsonb_build_object`, and last wins: every pulled row would carry a draw order where the client
// expects a cursor. It is named `drawOrder` on the wire for that reason, and this is the test that
// stops the next kind rediscovering it.
func TestNoKindClaimsAnEnvelopeKey(t *testing.T) {
	for _, kind := range SyncKindsInApplyOrder() {
		for reservedKey := range reservedChangeKeys {
			if kind.ClaimsField(reservedKey) {
				t.Errorf("kind %q claims the envelope key %q; give the field another name on the wire",
					kind.Name, reservedKey)
			}
		}
	}
}

// TestInkKindsHangFromAPage: an operation names strokes, but its *parent* is the page. Naming a
// stroke as a parent would make every erase wait for rows the server has no reason to have.
func TestInkKindsHangFromAPage(t *testing.T) {
	for _, name := range []string{"inkStroke", "inkErase", "inkMove"} {
		kind, known := SyncKindByName(name)
		if !known {
			t.Fatalf("kind %q is not registered", name)
		}
		if kind.ParentKind != "page" {
			t.Errorf("kind %q hangs from %q, want page", name, kind.ParentKind)
		}
	}
}

func TestAStrokeNeedsAPageAndASensibleDrawOrder(t *testing.T) {
	valid := func() *InkStrokeFields {
		return &InkStrokeFields{
			PageID:      "page-1",
			DrawOrder:   7,
			BrushFamily: "marker",
			Points:      []byte{1, 2, 3},
			Enc:         "ink/v1",
		}
	}

	if err := valid().Validate(); err != nil {
		t.Fatalf("an ordinary stroke was refused: %v", err)
	}

	missingPage := valid()
	missingPage.PageID = ""
	if err := missingPage.Validate(); err == nil {
		t.Error("a stroke with no page was accepted")
	}

	negative := valid()
	negative.DrawOrder = -1
	if err := negative.Validate(); err == nil {
		t.Error("a negative draw order was accepted")
	}

	// Equal draw orders are the *normal* offline case, not an error: two devices allocate the same
	// value because neither saw the other, and the client breaks the tie by id.
	shared := valid()
	shared.DrawOrder = 7
	if err := shared.Validate(); err != nil {
		t.Errorf("a stroke sharing a draw order was refused: %v", err)
	}

	huge := valid()
	huge.Points = make([]byte, maxInkPointsBytes+1)
	err := huge.Validate()
	if !errors.Is(err, ErrTooLarge) {
		t.Errorf("an oversized stroke gave %v, want a too-large error so it is rejected rather than retried", err)
	}
}

func TestAnOperationsTargetsAreCheckedButNeverResolved(t *testing.T) {
	erase := &InkEraseFields{
		PageID:    "page-1",
		Mode:      "Normal",
		Points:    []byte{1},
		TargetIDs: []string{"stroke-1", "stroke-that-was-purged"},
	}
	// An id naming no stored stroke is the point: the client replays an operation against what it
	// can find. Refusing it here would wedge a device whose target arrived in a later delta page.
	if err := erase.Validate(); err != nil {
		t.Fatalf("an operation naming an unknown stroke was refused: %v", err)
	}

	badTarget := &InkEraseFields{PageID: "page-1", Mode: "Normal", TargetIDs: []string{"has\x00nul"}}
	if err := badTarget.Validate(); err == nil {
		t.Error("a target id PostgreSQL cannot store was accepted, which would abort the whole batch")
	}

	tooMany := &InkMoveFields{PageID: "page-1", TargetIDs: make([]string, maxInkTargetsPerOperation+1)}
	if err := tooMany.Validate(); !errors.Is(err, ErrTooLarge) {
		t.Errorf("an unbounded target array gave %v, want a too-large error", err)
	}
}

// TestInkDefaultsItsEncoder: a client that says nothing about encoding means the format the app has
// always written, and the stored row has to say so rather than holding an empty string.
func TestInkDefaultsItsEncoder(t *testing.T) {
	stroke := &InkStrokeFields{PageID: "page-1", BrushFamily: "marker"}
	_, arguments := stroke.upsertStatement(ChangeEnvelope{}, 0)
	if !containsArgument(arguments, defaultInkEncoding) {
		t.Errorf("a stroke with no enc stored %v, want %q among them", arguments, defaultInkEncoding)
	}
}

// TestAnOperationWithNoTargetsStoresAnEmptyArray: `target_ids` is NOT NULL, and a nil slice would
// fail the whole transaction rather than the one entity.
func TestAnOperationWithNoTargetsStoresAnEmptyArray(t *testing.T) {
	erase := &InkEraseFields{PageID: "page-1", Mode: "Normal"}
	_, arguments := erase.upsertStatement(ChangeEnvelope{}, 0)
	for _, argument := range arguments {
		if targets, isSlice := argument.([]string); isSlice {
			if targets == nil {
				t.Error("an operation with no targets would write NULL into a NOT NULL column")
			}
			return
		}
	}
	t.Error("the erase upsert passes no target array at all")
}

// TestInkRendersItsPointsAsBase64: the pull expression is hand-written SQL, and a stroke returned as
// raw bytes would decode to nothing on every device.
func TestInkRendersItsPointsAsBase64(t *testing.T) {
	for _, name := range []string{"inkStroke", "inkErase", "inkMove"} {
		kind, _ := SyncKindByName(name)
		if !strings.Contains(kind.changeJSON, "encode(points, 'base64')") {
			t.Errorf("kind %q does not base64 its points on pull", name)
		}
	}
}

func containsArgument(arguments []any, wanted string) bool {
	for _, argument := range arguments {
		if text, isText := argument.(string); isText && text == wanted {
			return true
		}
	}
	return false
}
