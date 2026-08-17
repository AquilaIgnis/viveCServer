package tests

import (
	"encoding/base64"
	"fmt"
	"testing"
)

// TestTwoDevicesDrawingOnOnePageUnion is S4's exit criterion.
//
// Ink is the one part of this model that needs no conflict resolution at all: two devices draw, and
// the correct answer is both. What the server has to get right is that it never treats the pair as a
// conflict — including when they allocate the *same* draw order, which offline devices do routinely
// because each numbers its stroke above everything it has seen and neither has seen the other.
func TestTwoDevicesDrawingOnOnePageUnion(t *testing.T) {
	fixture := newSyncFixture(t)
	tablet := fixture.registerDevice("tablet")
	phone := fixture.registerDevice("phone")

	tablet.push(
		notebookChange("notebook-1", 0, "Sketches"),
		sectionChange("section-1", 0, "notebook-1", "Ideas"),
		pageChange("page-1", 0, "section-1", "Canvas"),
	)
	phone.pull(0, 0)

	// Both devices draw two strokes on the page while apart, from the same starting number.
	tablet.push(
		inkStrokeChange("stroke-t1", 0, "page-1", 7, []byte{1, 1}),
		inkStrokeChange("stroke-t2", 0, "page-1", 8, []byte{1, 2}),
	)
	phone.push(
		inkStrokeChange("stroke-p1", 0, "page-1", 7, []byte{2, 1}),
		inkStrokeChange("stroke-p2", 0, "page-1", 8, []byte{2, 2}),
	)

	tabletState, tabletCursor := tablet.pullEverything(0, 0)
	phoneState, phoneCursor := phone.pullEverything(0, 0)
	if tabletCursor != phoneCursor {
		t.Fatalf("cursors diverged: tablet %d, phone %d", tabletCursor, phoneCursor)
	}

	strokes := map[string]map[string]any{}
	for _, change := range tabletState {
		if change["kind"] == "inkStroke" {
			strokes[change["id"].(string)] = change
		}
	}
	if len(strokes) != 4 {
		t.Fatalf("the page holds %d strokes, want all 4 — nothing about ink is last-writer-wins", len(strokes))
	}
	for _, id := range []string{"stroke-t1", "stroke-p1"} {
		if strokes[id]["drawOrder"].(float64) != 7 {
			t.Errorf("%s came back with drawOrder %v, want the 7 its device allocated", id, strokes[id]["drawOrder"])
		}
	}

	tabletByID, phoneByID := changesByID(tabletState), changesByID(phoneState)
	for id, tabletChange := range tabletByID {
		if fmt.Sprint(tabletChange) != fmt.Sprint(phoneByID[id]) {
			t.Fatalf("devices diverged on %s:\n tablet %v\n phone  %v", id, tabletChange, phoneByID[id])
		}
	}
}

// TestAStrokeRoundTripsEveryFieldItWasDrawnWith.
//
// Every column here is hand-written twice — once in an INSERT, once in a JSON expression — so a
// mismatch stores a stroke and returns it looking like a different one. Floats especially: they
// travel as JSON numbers through a `real` column and have to come back as what was sent.
func TestAStrokeRoundTripsEveryFieldItWasDrawnWith(t *testing.T) {
	fixture := newSyncFixture(t)
	tablet := fixture.registerDevice("tablet")

	tablet.push(
		notebookChange("notebook-1", 0, "Sketches"),
		sectionChange("section-1", 0, "notebook-1", "Ideas"),
		pageChange("page-1", 0, "section-1", "Canvas"),
	)

	points := []byte{0x01, 0x7f, 0x80, 0xff, 0x00, 0x42}
	stroke := inkStrokeChange("stroke-1", 0, "page-1", 12, points)
	stroke["colorFollowsTheme"] = true
	stroke["groupId"] = "group-1"
	tablet.push(stroke)

	pulled := changeOfKind(t, mustPullKind(t, tablet, "inkStroke"), "inkStroke")

	if encoded, ok := pulled["points"].(string); !ok || encoded != base64.StdEncoding.EncodeToString(points) {
		t.Errorf("points came back as %v, want the base64 of what was sent", pulled["points"])
	}
	for field, want := range map[string]float64{
		"drawOrder": 12, "brushVersion": 1, "sizeDp": 3.5, "colorArgb": -16777216,
		"epsilon": 0.1, "stabilization": 2, "minX": 1.5, "minY": 2.5, "maxX": 3.5, "maxY": 4.5,
	} {
		if got, ok := pulled[field].(float64); !ok || got != want {
			t.Errorf("%s came back as %v, want %v", field, pulled[field], want)
		}
	}
	if pulled["brushFamily"] != "pressure-pen" || pulled["enc"] != "ink/v1" {
		t.Errorf("brush %v / enc %v, want pressure-pen and ink/v1", pulled["brushFamily"], pulled["enc"])
	}
	if pulled["colorFollowsTheme"] != true || pulled["groupId"] != "group-1" {
		t.Errorf("colorFollowsTheme %v, groupId %v", pulled["colorFollowsTheme"], pulled["groupId"])
	}

	// The tri-state has to survive as three states: absent is "the app never recorded the intent",
	// which is not the same as "the user chose this colour deliberately".
	plain := inkStrokeChange("stroke-2", 0, "page-1", 13, points)
	tablet.push(plain)
	for _, change := range mustPullKind(t, tablet, "inkStroke") {
		if change["id"] == "stroke-2" && change["colorFollowsTheme"] != nil {
			t.Errorf("an unrecorded colour intent came back as %v, want null", change["colorFollowsTheme"])
		}
	}
}

// TestAnOperationCarriesItsTargetsIncludingOnesNobodyHas.
//
// The mask and its targets are one entity because the pair is unsafe apart: replayed against every
// stroke on the page, an erase would also cut ink drawn after it. And a target naming no stored
// stroke has to round-trip untouched — the app's own seven-day purge removes strokes that operations
// still name, and a server that dropped or refused those ids would quietly rewrite an operation
// that is supposed to be immutable.
func TestAnOperationCarriesItsTargetsIncludingOnesNobodyHas(t *testing.T) {
	fixture := newSyncFixture(t)
	tablet := fixture.registerDevice("tablet")
	phone := fixture.registerDevice("phone")

	tablet.push(
		notebookChange("notebook-1", 0, "Sketches"),
		sectionChange("section-1", 0, "notebook-1", "Ideas"),
		pageChange("page-1", 0, "section-1", "Canvas"),
		inkStrokeChange("stroke-1", 0, "page-1", 0, []byte{1}),
	)
	tablet.push(
		inkEraseChange("erase-1", 0, "page-1", []string{"stroke-1", "stroke-long-purged"}),
		inkMoveChange("move-1", 0, "page-1", []string{"stroke-1"}),
	)

	state, _ := phone.pullEverything(0, 0)
	erase := changeOfKind(t, state, "inkErase")
	move := changeOfKind(t, state, "inkMove")

	if fmt.Sprint(erase["targetIds"]) != "[stroke-1 stroke-long-purged]" {
		t.Errorf("erase targets came back as %v, want both ids in the order they were sent", erase["targetIds"])
	}
	if fmt.Sprint(move["targetIds"]) != "[stroke-1]" {
		t.Errorf("move targets came back as %v", move["targetIds"])
	}
	if erase["mode"] != "Normal" || erase["sizeDp"].(float64) != 8 {
		t.Errorf("erase came back as mode %v size %v", erase["mode"], erase["sizeDp"])
	}
	if move["dxDp"].(float64) != 12.5 || move["dyDp"].(float64) != -4.25 || move["scaleX"].(float64) != 1 {
		t.Errorf("move came back as %v, %v, scale %v", move["dxDp"], move["dyDp"], move["scaleX"])
	}
}

// TestErasingAndUndoingAreOrdinaryVersionedWrites.
//
// This is why ink takes the same optimistic-concurrency path as every other kind rather than SD4's
// insert-if-absent fast path. A stroke's tombstone is not monotonic: erasing sets `deletedAt` and
// undoing clears it again, and an insert-only path would silently drop both.
func TestErasingAndUndoingAreOrdinaryVersionedWrites(t *testing.T) {
	fixture := newSyncFixture(t)
	tablet := fixture.registerDevice("tablet")
	phone := fixture.registerDevice("phone")

	tablet.push(
		notebookChange("notebook-1", 0, "Sketches"),
		sectionChange("section-1", 0, "notebook-1", "Ideas"),
		pageChange("page-1", 0, "section-1", "Canvas"),
		inkStrokeChange("stroke-1", 0, "page-1", 0, []byte{1}),
	)

	erased := inkStrokeChange("stroke-1", 1, "page-1", 0, []byte{1})
	erased["deletedAt"] = 1_700_000_500_000
	if result := tablet.push(erased); len(result.Applied) != 1 || result.Applied[0].Version != 2 {
		t.Fatalf("erasing a stroke: applied %+v rejected %+v", result.Applied, result.Rejected)
	}
	if pulled := changeOfKind(t, mustPullKind(t, phone, "inkStroke"), "inkStroke"); pulled["deletedAt"] == nil {
		t.Fatal("the tombstone never reached the other device")
	}

	restored := inkStrokeChange("stroke-1", 2, "page-1", 0, []byte{1})
	restored["deletedAt"] = nil
	if result := tablet.push(restored); len(result.Applied) != 1 || result.Applied[0].Version != 3 {
		t.Fatalf("undoing the erase: applied %+v rejected %+v", result.Applied, result.Rejected)
	}
	pulled := changeOfKind(t, mustPullKind(t, phone, "inkStroke"), "inkStroke")
	if pulled["deletedAt"] != nil {
		t.Fatalf("the undo did not reach the other device: deletedAt is %v", pulled["deletedAt"])
	}

	// A recolour racing the same row is an ordinary conflict, and the loser gets the current state.
	stale := inkStrokeChange("stroke-1", 2, "page-1", 0, []byte{1})
	stale["colorArgb"] = -65536
	result := phone.push(stale)
	if len(result.Rejected) != 1 || result.Rejected[0].Reason != "version_conflict" {
		t.Fatalf("a stale ink write gave %+v, want one version_conflict", result)
	}
	if result.Rejected[0].Current == nil {
		t.Fatal("the conflict carried no current state, so the client cannot rebase without asking again")
	}
}

// TestInkWaitsForItsPage: a stroke whose page has never been uploaded is `missing_parent`, exactly
// like every other child kind, and the rest of the batch still lands.
func TestInkWaitsForItsPage(t *testing.T) {
	fixture := newSyncFixture(t)
	tablet := fixture.registerDevice("tablet")

	tablet.push(
		notebookChange("notebook-1", 0, "Sketches"),
		sectionChange("section-1", 0, "notebook-1", "Ideas"),
		pageChange("page-1", 0, "section-1", "Canvas"),
	)

	result := tablet.push(
		inkStrokeChange("stroke-1", 0, "page-1", 0, []byte{1}),
		inkStrokeChange("stroke-2", 0, "page-missing", 0, []byte{2}),
		inkEraseChange("erase-1", 0, "page-missing", []string{"stroke-1"}),
	)
	if len(result.Applied) != 1 || result.Applied[0].ID != "stroke-1" {
		t.Fatalf("applied %+v, want only the stroke on the page that exists", result.Applied)
	}
	if len(result.Rejected) != 2 {
		t.Fatalf("rejected %+v, want both entities on the missing page", result.Rejected)
	}
	for _, rejection := range result.Rejected {
		if rejection.Reason != "missing_parent" {
			t.Errorf("%s %s was rejected %q, want missing_parent", rejection.Kind, rejection.ID, rejection.Reason)
		}
	}

	// A stroke and the page it needs may travel in one batch, because kinds apply in rank order.
	together := tablet.push(
		inkStrokeChange("stroke-3", 0, "page-2", 0, []byte{3}),
		pageChange("page-2", 0, "section-1", "Second"),
	)
	if len(together.Applied) != 2 {
		t.Fatalf("a page and a stroke in one batch: applied %+v rejected %+v", together.Applied, together.Rejected)
	}
}

// TestATenThousandStrokePageSyncsBothWays is the volume half of S4's exit criterion.
//
// Ink is where the data actually is — a notebook may hold 500,000 strokes against a few thousand
// rows of everything else — so the interesting question is not whether one stroke round-trips but
// whether a drawn page moves through batches of 512 and comes back whole.
func TestATenThousandStrokePageSyncsBothWays(t *testing.T) {
	if testing.Short() {
		t.Skip("volume test")
	}

	const strokes = 10_000
	const batchSize = 512

	fixture := newSyncFixture(t)
	tablet := fixture.registerDevice("tablet")
	phone := fixture.registerDevice("phone")

	tablet.push(
		notebookChange("notebook-1", 0, "Sketches"),
		sectionChange("section-1", 0, "notebook-1", "Ideas"),
		pageChange("page-1", 0, "section-1", "Canvas"),
	)

	points := make([]byte, 64)
	for index := range points {
		points[index] = byte(index)
	}
	for start := 0; start < strokes; start += batchSize {
		batch := make([]map[string]any, 0, batchSize)
		for offset := 0; offset < batchSize && start+offset < strokes; offset++ {
			number := start + offset
			batch = append(batch, inkStrokeChange(fmt.Sprintf("stroke-%05d", number), 0, "page-1", number, points))
		}
		if result := tablet.push(batch...); len(result.Applied) != len(batch) {
			t.Fatalf("batch at %d applied %d of %d: %+v", start, len(result.Applied), len(batch), result.Rejected)
		}
	}

	state, cursor := phone.pullEverything(0, 0)
	pulledStrokes := 0
	for _, change := range state {
		if change["kind"] == "inkStroke" {
			pulledStrokes++
		}
	}
	if pulledStrokes != strokes {
		t.Fatalf("the other device pulled %d strokes of %d", pulledStrokes, strokes)
	}
	if cursor != tablet.cursor() {
		t.Fatalf("cursor after a large page: %d, want %d", cursor, tablet.cursor())
	}
}

// mustPullKind pulls everything and fails if the kind is absent, which keeps each assertion above
// about the field it names rather than about whether anything arrived.
func mustPullKind(t *testing.T, device *deviceClient, kind string) []map[string]any {
	t.Helper()
	state, _ := device.pullEverything(0, 0)
	for _, change := range state {
		if change["kind"] == kind {
			return state
		}
	}
	t.Fatalf("no %s reached this device", kind)
	return nil
}
