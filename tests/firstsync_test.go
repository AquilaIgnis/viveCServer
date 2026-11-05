package tests

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/AquilaIgnis/viveCServer/internal/store"
	"github.com/AquilaIgnis/viveCServer/internal/syncengine"
)

const skipClosedNotebook = "skipNotebook=notebook-closed"

// pullFiltered is one pull with extra query parameters, and the status it got.
func (device *deviceClient) pullFiltered(sinceCursor int64, limit int, query string) (syncengine.PullResult, int) {
	device.t.Helper()

	path := fmt.Sprintf("/v1/changes?since=%d&limit=%d&%s", sinceCursor, limit, query)
	var result syncengine.PullResult
	status, err := device.fixture.tryRequest(http.MethodGet, path, device.token, nil, &result)
	if err != nil {
		device.t.Fatalf("%s filtered pull: %v", device.name, err)
	}
	return result, status
}

// pullFilteredEverything drains a filtered pull the way a client does, and refuses the one answer a
// client treats as a broken server: an empty page with more to come.
func (device *deviceClient) pullFilteredEverything(limit int, query string) ([]map[string]any, int64) {
	device.t.Helper()

	collected := make([]map[string]any, 0)
	cursor := int64(0)
	for range 200 {
		page, status := device.pullFiltered(cursor, limit, query)
		if status != http.StatusOK {
			device.t.Fatalf("%s filtered pull from %d: status %d", device.name, cursor, status)
		}
		if page.HasMore && len(page.Changes) == 0 {
			device.t.Fatalf("%s got an empty page with more to come at cursor %d", device.name, page.Cursor)
		}
		for _, raw := range page.Changes {
			collected = append(collected, decodeChangeObject(device.t, raw))
		}
		cursor = page.Cursor
		if !page.HasMore {
			return collected, cursor
		}
	}
	device.t.Fatal("a filtered pull did not finish within 200 pages")
	return nil, 0
}

// keysOf names each change by kind and id, because a page and its body share an id.
func keysOf(changes []map[string]any) map[string]int {
	keys := make(map[string]int, len(changes))
	for _, change := range changes {
		keys[fmt.Sprintf("%v/%v", change["kind"], change["id"])]++
	}
	return keys
}

func assertExactKeys(t *testing.T, changes []map[string]any, want ...string) {
	t.Helper()

	got := keysOf(changes)
	for _, key := range want {
		if got[key] != 1 {
			t.Errorf("%s arrived %d times, want once", key, got[key])
		}
		delete(got, key)
	}
	for key := range got {
		t.Errorf("%s arrived and should not have", key)
	}
}

// pushOpenAndClosedNotebooks makes the account a joining device meets: an older notebook that was
// closed later, so its row sorts behind its own ink, and a newer open one.
func pushOpenAndClosedNotebooks(tablet *deviceClient) {
	tablet.t.Helper()

	tablet.push(
		notebookChange("notebook-closed", 0, "Archive"),
		sectionChange("section-closed", 0, "notebook-closed", "Old"),
		pageChange("page-closed", 0, "section-closed", "Old page"),
		pageContentChange("page-closed", 0, []byte(`{"blocks":[]}`)),
		inkStrokeChange("stroke-closed", 0, "page-closed", 0, []byte("closed ink")),
		inkEraseChange("erase-closed", 0, "page-closed", []string{"stroke-closed"}),
		inkMoveChange("move-closed", 0, "page-closed", []string{"stroke-closed"}),
	)
	tablet.push(
		notebookChange("notebook-open", 0, "Current"),
		sectionChange("section-open", 0, "notebook-open", "New"),
		pageChange("page-open", 0, "section-open", "New page"),
		pageContentChange("page-open", 0, []byte(`{"blocks":[]}`)),
		inkStrokeChange("stroke-open", 0, "page-open", 0, []byte("open ink")),
		inkEraseChange("erase-open", 0, "page-open", []string{"stroke-open"}),
		inkMoveChange("move-open", 0, "page-open", []string{"stroke-open"}),
	)
	tablet.push(closedNotebookChange("notebook-closed", 1, "Archive", closedAtMillis))
}

// TestATreeReadCarriesTheTreeAndNothingUnderIt is what lets a joining device know which notebooks
// are closed before it has spent anything on their content.
func TestATreeReadCarriesTheTreeAndNothingUnderIt(t *testing.T) {
	fixture := newSyncFixture(t)
	tablet := fixture.registerDevice("Studio tablet")
	phone := fixture.registerDevice("Phone")
	pushOpenAndClosedNotebooks(tablet)

	changes, cursor := phone.pullFilteredEverything(512, "scope=tree")

	assertExactKeys(t, changes,
		"notebook/notebook-closed", "section/section-closed", "page/page-closed",
		"notebook/notebook-open", "section/section-open", "page/page-open",
	)
	if cursor != phone.cursor() {
		t.Fatalf("the tree read ended at %d, want the account's cursor %d", cursor, phone.cursor())
	}
	if changesByID(changes)["notebook-closed"]["closedAt"] != float64(closedAtMillis) {
		t.Fatalf("the tree read does not say the notebook is closed: %+v", changesByID(changes)["notebook-closed"])
	}
}

// TestATreeReadPagesWithoutRepeatingOrLosingARow: page boundaries come from the tree's own rows, so
// a run of content between two tree rows neither ends the read nor shows up as an empty page.
func TestATreeReadPagesWithoutRepeatingOrLosingARow(t *testing.T) {
	fixture := newSyncFixture(t)
	tablet := fixture.registerDevice("Studio tablet")
	phone := fixture.registerDevice("Phone")

	tablet.push(
		notebookChange("notebook-1", 0, "Work"),
		sectionChange("section-1", 0, "notebook-1", "Plans"),
		pageChange("page-1", 0, "section-1", "Monday"),
	)
	for stroke := range 5 {
		tablet.push(inkStrokeChange(fmt.Sprintf("stroke-%d", stroke), 0, "page-1", stroke, []byte("ink")))
	}
	tablet.push(pageChange("page-2", 0, "section-1", "Tuesday"))
	tablet.push(notebookChange("notebook-2", 0, "Home"))

	changes, _ := phone.pullFilteredEverything(1, "scope=tree")

	assertExactKeys(t, changes,
		"notebook/notebook-1", "section/section-1", "page/page-1", "page/page-2", "notebook/notebook-2",
	)
}

// TestATreeReadAcknowledgesNothing: `since` on an ordinary pull is proof the device committed that
// far, and it decides when the account may forget a purge. A position in a scan proves no such thing.
func TestATreeReadAcknowledgesNothing(t *testing.T) {
	fixture := newSyncFixture(t)
	tablet := fixture.registerDevice("Studio tablet")
	phone := fixture.registerDevice("Phone")
	pushOpenAndClosedNotebooks(tablet)
	cursor := phone.cursor()

	acknowledged := func() int64 {
		t.Helper()
		var seq int64
		if err := fixture.pool.QueryRow(context.Background(),
			`SELECT last_pulled_seq FROM devices WHERE id = $1::uuid`, phone.deviceID,
		).Scan(&seq); err != nil {
			t.Fatalf("reading the acknowledged cursor: %v", err)
		}
		return seq
	}

	if _, status := phone.pullFiltered(cursor, 512, "scope=tree"); status != http.StatusOK {
		t.Fatalf("tree read: status %d", status)
	}
	if got := acknowledged(); got != 0 {
		t.Fatalf("a tree read acknowledged cursor %d", got)
	}

	if _, status := phone.pullFiltered(cursor, 512, skipClosedNotebook); status != http.StatusOK {
		t.Fatalf("filtered pull: status %d", status)
	}
	if got := acknowledged(); got != cursor {
		t.Fatalf("a filtered pull acknowledged %d, want %d: it commits a cursor like any other", got, cursor)
	}
}

// TestATreeReadCarriesNoPurgesAndTheDeltaDoes: no window of a tree read is ever committed, so a purge
// reported there would be one the device could lose.
func TestATreeReadCarriesNoPurgesAndTheDeltaDoes(t *testing.T) {
	fixture := newSyncFixture(t)
	tablet := fixture.registerDevice("Studio tablet")
	phone := fixture.registerDevice("Phone")

	tablet.push(notebookChange("notebook-gone", 0, "Old"))
	deletion := notebookChange("notebook-gone", 1, "Old")
	deletion["deletedAt"] = deletionAtMillis
	tablet.push(deletion)
	if err := store.DeleteArchivedNotebook(
		context.Background(), fixture.pool, fixture.accountID, "notebook-gone",
	); err != nil {
		t.Fatalf("permanently deleting the archive: %v", err)
	}

	tree, status := phone.pullFiltered(0, 512, "scope=tree")
	if status != http.StatusOK {
		t.Fatalf("tree read: status %d", status)
	}
	if len(tree.Purges) != 0 {
		t.Fatalf("a tree read carried purges: %s", tree.Purges)
	}

	skipping, status := phone.pullFiltered(0, 512, skipClosedNotebook)
	if status != http.StatusOK {
		t.Fatalf("filtered pull: status %d", status)
	}
	if len(skipping.Purges) != 1 {
		t.Fatalf("a filtered pull carried %d purges, want 1: it commits a cursor over them", len(skipping.Purges))
	}
}

// TestSkippingANotebookLeavesOutItsContentAndNothingElse is the download a joining device no longer
// makes. The cursor still reaches the end, so the next ordinary pull has nothing to repeat.
func TestSkippingANotebookLeavesOutItsContentAndNothingElse(t *testing.T) {
	fixture := newSyncFixture(t)
	tablet := fixture.registerDevice("Studio tablet")
	phone := fixture.registerDevice("Phone")
	pushOpenAndClosedNotebooks(tablet)

	changes, cursor := phone.pullFilteredEverything(512, skipClosedNotebook)

	assertExactKeys(t, changes,
		"notebook/notebook-closed", "section/section-closed", "page/page-closed",
		"notebook/notebook-open", "section/section-open", "page/page-open",
		"pageContent/page-open", "inkStroke/stroke-open", "inkErase/erase-open", "inkMove/move-open",
	)
	if cursor != phone.cursor() {
		t.Fatalf("the filtered pull ended at %d, want the account's cursor %d", cursor, phone.cursor())
	}
	if rest := phone.pull(cursor, 512); len(rest.Changes) != 0 || rest.HasMore {
		t.Fatalf("an ordinary pull from there found more: %+v", rest)
	}
}

// TestAWholeNotebookWithItsContentStillArrivesWithoutTheFilter guards the default: an unfiltered pull
// is what restoring a notebook reads, and it has to go on carrying everything.
func TestAWholeNotebookWithItsContentStillArrivesWithoutTheFilter(t *testing.T) {
	fixture := newSyncFixture(t)
	tablet := fixture.registerDevice("Studio tablet")
	phone := fixture.registerDevice("Phone")
	pushOpenAndClosedNotebooks(tablet)

	changes, _ := phone.pullEverything(0, 512)

	assertExactKeys(t, changes,
		"notebook/notebook-closed", "section/section-closed", "page/page-closed",
		"pageContent/page-closed", "inkStroke/stroke-closed", "inkErase/erase-closed", "inkMove/move-closed",
		"notebook/notebook-open", "section/section-open", "page/page-open",
		"pageContent/page-open", "inkStroke/stroke-open", "inkErase/erase-open", "inkMove/move-open",
	)
}

// TestPagingAroundSkippedContentNeverAnswersEmpty: the boundary query has to apply the filter too.
// Chosen from rows the read then leaves out, a page would come back empty with more to come, and the
// client stops syncing on that answer. The tail is all skipped rows, so the last answer has to be
// "nothing more" rather than a page that never ends.
func TestPagingAroundSkippedContentNeverAnswersEmpty(t *testing.T) {
	fixture := newSyncFixture(t)
	tablet := fixture.registerDevice("Studio tablet")
	phone := fixture.registerDevice("Phone")
	pushOpenAndClosedNotebooks(tablet)

	for stroke := range 4 {
		tablet.push(inkStrokeChange(fmt.Sprintf("closed-%d", stroke), 0, "page-closed", stroke+1, []byte("ink")))
		tablet.push(inkStrokeChange(fmt.Sprintf("open-%d", stroke), 0, "page-open", stroke+1, []byte("ink")))
	}
	for stroke := 4; stroke < 8; stroke++ {
		tablet.push(inkStrokeChange(fmt.Sprintf("closed-%d", stroke), 0, "page-closed", stroke+1, []byte("ink")))
	}

	changes, cursor := phone.pullFilteredEverything(1, skipClosedNotebook)

	if cursor != phone.cursor() {
		t.Fatalf("the filtered pull ended at %d, want the account's cursor %d", cursor, phone.cursor())
	}
	keys := keysOf(changes)
	for stroke := range 4 {
		if key := fmt.Sprintf("inkStroke/open-%d", stroke); keys[key] != 1 {
			t.Errorf("%s arrived %d times, want once", key, keys[key])
		}
	}
	for key := range keys {
		if strings.HasPrefix(key, "inkStroke/closed-") {
			t.Errorf("%s arrived from a skipped notebook", key)
		}
	}
}

// TestSkippingJudgesAPageByWhereItIsNow: a page moved out of a closed notebook is in an open one, and
// withholding its content on the strength of where it used to be would leave it empty for good.
func TestSkippingJudgesAPageByWhereItIsNow(t *testing.T) {
	fixture := newSyncFixture(t)
	tablet := fixture.registerDevice("Studio tablet")
	phone := fixture.registerDevice("Phone")
	pushOpenAndClosedNotebooks(tablet)

	tablet.push(pageChange("page-closed", 1, "section-open", "Moved out"))
	tablet.push(pageChange("page-open", 1, "section-closed", "Moved in"))

	changes, _ := phone.pullFilteredEverything(512, skipClosedNotebook)

	keys := keysOf(changes)
	for _, key := range []string{"pageContent/page-closed", "inkStroke/stroke-closed"} {
		if keys[key] != 1 {
			t.Errorf("%s arrived %d times, want once: its page is in an open notebook now", key, keys[key])
		}
	}
	for _, key := range []string{"pageContent/page-open", "inkStroke/stroke-open"} {
		if keys[key] != 0 {
			t.Errorf("%s arrived: its page is in the skipped notebook now", key)
		}
	}
}

// TestSkippingANotebookStillDeliversAttachments: a picture belongs to no notebook, so the server
// cannot tell whose it is. The client decides from the bodies it keeps.
func TestSkippingANotebookStillDeliversAttachments(t *testing.T) {
	fixture := newSyncFixture(t)
	tablet := fixture.registerDevice("Studio tablet")
	phone := fixture.registerDevice("Phone")
	pushOpenAndClosedNotebooks(tablet)

	picture := pictureBytes(0x21, 4*1024)
	digest := digestOf(picture)
	if upload := tablet.uploadBlob(digest, picture); upload.status >= http.StatusBadRequest {
		t.Fatalf("uploading the picture: status %d", upload.status)
	}
	tablet.push(attachmentChange(digest, 0, int64(len(picture))))

	changes, _ := phone.pullFilteredEverything(512, skipClosedNotebook)

	if keysOf(changes)["attachment/"+digest] != 1 {
		t.Fatalf("the attachment did not arrive through a filtered pull")
	}
}

// TestAFirstPullFromTheCurrentAppIsAnswered sends exactly what the app sends. This server has no
// cursor floor and no reconciliation, and the app does not know which kind of server it is talking
// to, so `reconcile=true` has to be ignored rather than refused. A refusal here would stop every
// first sync against a community server, and it is the one thing no test on the client side can see.
func TestAFirstPullFromTheCurrentAppIsAnswered(t *testing.T) {
	fixture := newSyncFixture(t)
	tablet := fixture.registerDevice("Studio tablet")
	phone := fixture.registerDevice("Phone")
	pushOpenAndClosedNotebooks(tablet)
	cursor := phone.cursor()

	tree, status := phone.pullFiltered(0, 512, "scope=tree")
	if status != http.StatusOK {
		t.Fatalf("the tree read: status %d", status)
	}
	if tree.Cursor != cursor || tree.HasMore {
		t.Fatalf("the tree read ended at %d with more=%v, want %d", tree.Cursor, tree.HasMore, cursor)
	}

	changes, ended := phone.pullFilteredEverything(2048, skipClosedNotebook+"&reconcile=true")

	if ended != cursor {
		t.Fatalf("the first pull ended at %d, want %d", ended, cursor)
	}
	assertExactKeys(t, changes,
		"notebook/notebook-closed", "section/section-closed", "page/page-closed",
		"notebook/notebook-open", "section/section-open", "page/page-open",
		"pageContent/page-open", "inkStroke/stroke-open", "inkErase/erase-open", "inkMove/move-open",
	)
}

// TestAFilterThatCannotMeanAnythingIsRefused: each of these would otherwise read as something else.
func TestAFilterThatCannotMeanAnythingIsRefused(t *testing.T) {
	fixture := newSyncFixture(t)
	phone := fixture.registerDevice("Phone")

	tooMany := url.Values{}
	for index := range 257 {
		tooMany.Add("skipNotebook", fmt.Sprintf("notebook-%d", index))
	}

	for name, query := range map[string]string{
		"an unknown scope":         "scope=everything",
		"a tree read that skips":   "scope=tree&" + skipClosedNotebook,
		"an empty notebook id":     "skipNotebook=",
		"more ids than are served": tooMany.Encode(),
	} {
		if _, status := phone.pullFiltered(0, 512, query); status != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", name, status)
		}
	}
}
