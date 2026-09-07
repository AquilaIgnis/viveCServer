package tests

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AquilaIgnis/viveCServer/internal/syncengine"
)

// TestChangeStreamDeliversContentOnlyToPeers proves the HTTP wiring, commit boundary, author
// exclusion, and idempotent-replay suppression together. The peer receives the authoritative delta
// in the stream itself; it makes no cursor or pull request after the push.
func TestChangeStreamDeliversContentOnlyToPeers(t *testing.T) {
	fixture := newSyncFixture(t)
	tablet := fixture.registerDevice("tablet")
	phone := fixture.registerDevice("phone")

	tabletStream := openChangeStream(t, tablet)
	defer tabletStream.close()
	phoneStream := openChangeStream(t, phone)
	defer phoneStream.close()
	if event := tabletStream.read(t); event != "ready" {
		t.Fatalf("tablet's first stream event = %q, want ready", event)
	}
	if event := phoneStream.read(t); event != "ready" {
		t.Fatalf("phone's first stream event = %q, want ready", event)
	}

	batchID := newBatchID(t)
	created := tablet.pushBatch(batchID, notebookChange("notebook-stream", 0, "Streamed"))
	if len(created.Applied) != 1 {
		t.Fatalf("push applied %+v, want one change", created.Applied)
	}
	event := phoneStream.readFrame(t)
	if event.name != "changes" {
		t.Fatalf("peer stream event = %q, want changes", event.name)
	}
	var streamed syncengine.PullResult
	if err := json.Unmarshal(event.data, &streamed); err != nil {
		t.Fatalf("decoding streamed delta: %v", err)
	}
	if len(streamed.Changes) != 1 || streamed.Cursor != created.Cursor || streamed.HasMore {
		t.Fatalf("streamed delta = %+v, want the committed notebook through cursor %d", streamed, created.Cursor)
	}
	if got := decodeChangeObject(t, streamed.Changes[0])["id"]; got != "notebook-stream" {
		t.Fatalf("streamed id = %v, want notebook-stream", got)
	}
	if event, arrived := tabletStream.tryRead(); arrived {
		t.Fatalf("author stream received %q", event)
	}

	// The exact batch is replayed from applied_batches, not committed again, so it must not wake
	// the peer a second time even though the HTTP response still lists its original applied row.
	tablet.pushBatch(batchID, notebookChange("notebook-stream", 0, "Streamed"))
	if event, arrived := phoneStream.tryRead(); arrived {
		t.Fatalf("idempotent replay emitted %q", event)
	}
}

func TestRevokingADeviceEndsItsAuthenticatedStream(t *testing.T) {
	fixture := newSyncFixture(t)
	tablet := fixture.registerDevice("tablet")
	phone := fixture.registerDevice("phone")
	phoneStream := openChangeStream(t, phone)
	defer phoneStream.close()
	if event := phoneStream.read(t); event != "ready" {
		t.Fatalf("phone's first stream event = %q, want ready", event)
	}

	status := fixture.request(http.MethodDelete, "/v1/devices/"+phone.deviceID, tablet.token, nil, nil)
	if status != http.StatusNoContent {
		t.Fatalf("revoking phone returned %d, want 204", status)
	}
	if event := phoneStream.read(t); event != "revoked" {
		t.Fatalf("revoked phone stream event = %q, want revoked", event)
	}
}

func TestChangeStreamReadyCarriesReconnectBacklog(t *testing.T) {
	fixture := newSyncFixture(t)
	writer := fixture.registerDevice("writer")
	reader := fixture.registerDevice("reader")
	created := writer.push(notebookChange("offline-change", 0, "Arrived while away"))

	stream := openChangeStreamAt(t, reader, 0)
	defer stream.close()
	ready := stream.readFrame(t)
	if ready.name != "ready" {
		t.Fatalf("first stream event = %q, want ready", ready.name)
	}
	var backlog syncengine.PullResult
	if err := json.Unmarshal(ready.data, &backlog); err != nil {
		t.Fatalf("decoding ready backlog: %v", err)
	}
	if len(backlog.Changes) != 1 || backlog.Cursor != created.Cursor {
		t.Fatalf("ready backlog = %+v, want one change through cursor %d", backlog, created.Cursor)
	}
}

type testChangeStream struct {
	cancel context.CancelFunc
	body   io.ReadCloser
	reader *bufio.Reader
}

type changeStreamFrame struct {
	name string
	data []byte
}

func openChangeStream(t *testing.T, device *deviceClient) *testChangeStream {
	return openChangeStreamAt(t, device, 0)
}

func openChangeStreamAt(t *testing.T, device *deviceClient, since int64) *testChangeStream {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodGet,
		fmt.Sprintf("%s/v1/changes/watch?since=%d", device.fixture.baseURL, since),
		nil,
	)
	if err != nil {
		cancel()
		t.Fatalf("building change stream request: %v", err)
	}
	request.Header.Set("Authorization", "Bearer "+device.token)
	request.Header.Set("Accept", "text/event-stream")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		cancel()
		t.Fatalf("opening change stream: %v", err)
	}
	if response.StatusCode != http.StatusOK || response.Header.Get("Content-Type") != "text/event-stream" {
		response.Body.Close()
		cancel()
		t.Fatalf("change stream answered %d %q", response.StatusCode, response.Header.Get("Content-Type"))
	}
	return &testChangeStream{cancel: cancel, body: response.Body, reader: bufio.NewReader(response.Body)}
}

func (stream *testChangeStream) close() {
	stream.cancel()
	_ = stream.body.Close()
}

func (stream *testChangeStream) read(t *testing.T) string {
	return stream.readFrame(t).name
}

func (stream *testChangeStream) readFrame(t *testing.T) changeStreamFrame {
	t.Helper()
	event, err := readChangeStreamEvent(stream.reader)
	if err != nil {
		t.Fatalf("reading change stream: %v", err)
	}
	return event
}

func (stream *testChangeStream) tryRead() (string, bool) {
	result := make(chan string, 1)
	go func() {
		event, _ := readChangeStreamEvent(stream.reader)
		result <- event.name
	}()
	select {
	case event := <-result:
		return event, true
	case <-time.After(100 * time.Millisecond):
		return "", false
	}
}

func readChangeStreamEvent(reader *bufio.Reader) (changeStreamFrame, error) {
	eventName := ""
	data := make([]byte, 0)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return changeStreamFrame{}, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			return changeStreamFrame{name: eventName, data: data}, nil
		}
		if strings.HasPrefix(line, "event:") {
			eventName = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		}
		if strings.HasPrefix(line, "data:") {
			if len(data) > 0 {
				data = append(data, '\n')
			}
			data = append(data, strings.TrimSpace(strings.TrimPrefix(line, "data:"))...)
		}
	}
}

// TestTwoDevicesConvergeOnTheHierarchy is S2's exit criterion: two simulated clients converge on
// notebooks, sections and pages, including a deletion.
func TestTwoDevicesConvergeOnTheHierarchy(t *testing.T) {
	fixture := newSyncFixture(t)
	tablet := fixture.registerDevice("tablet")
	phone := fixture.registerDevice("phone")

	// One batch carrying a whole tree. The section and page name parents that are arriving in the
	// same push, which only works because the engine applies kinds in order.
	created := tablet.push(
		pageChange("page-1", 0, "section-1", "Monday"),
		notebookChange("notebook-1", 0, "Work"),
		sectionChange("section-1", 0, "notebook-1", "Meetings"),
	)
	if len(created.Applied) != 3 || len(created.Rejected) != 0 {
		t.Fatalf("first push applied %d rejected %d, want 3 and 0: %+v", len(created.Applied), len(created.Rejected), created.Rejected)
	}
	if created.Cursor != 1 {
		t.Fatalf("first push cursor = %d, want 1", created.Cursor)
	}

	// The whole tree carries one sequence value: a batch is atomic across kinds (SD2).
	delta := phone.pull(0, 0)
	if len(delta.Changes) != 3 || delta.Cursor != 1 || delta.HasMore {
		t.Fatalf("phone pull = %d changes, cursor %d, hasMore %v; want 3, 1, false", len(delta.Changes), delta.Cursor, delta.HasMore)
	}
	kindsInOrder := make([]string, 0, 3)
	for _, raw := range delta.Changes {
		change := decodeChangeObject(t, raw)
		kindsInOrder = append(kindsInOrder, change["kind"].(string))
		if change["seq"].(float64) != 1 {
			t.Fatalf("change %v carries seq %v, want 1", change["id"], change["seq"])
		}
	}
	if fmt.Sprint(kindsInOrder) != "[notebook section page]" {
		t.Fatalf("pull order = %v, want parents before children", kindsInOrder)
	}

	// The phone edits the page it just learned about.
	renamed := phone.push(pageChange("page-1", 1, "section-1", "Monday standup"))
	if len(renamed.Applied) != 1 || renamed.Applied[0].Version != 2 {
		t.Fatalf("rename applied = %+v, want one entity at version 2", renamed.Applied)
	}

	// The tablet sees only what changed since its own push.
	tabletDelta := tablet.pull(1, 0)
	if len(tabletDelta.Changes) != 1 {
		t.Fatalf("tablet delta = %d changes, want 1", len(tabletDelta.Changes))
	}
	renamedPage := decodeChangeObject(t, tabletDelta.Changes[0])
	if renamedPage["title"] != "Monday standup" || renamedPage["version"].(float64) != 2 {
		t.Fatalf("tablet saw %+v, want the renamed page at version 2", renamedPage)
	}

	// A delete is an ordinary write that sets deletedAt, so it travels like any other change.
	deletion := pageChange("page-1", 2, "section-1", "Monday standup")
	deletion["deletedAt"] = 1_700_000_500_000
	if applied := tablet.push(deletion); len(applied.Applied) != 1 {
		t.Fatalf("deleting the page: %+v", applied.Rejected)
	}

	tabletState, tabletCursor := tablet.pullEverything(0, 0)
	phoneState, phoneCursor := phone.pullEverything(0, 0)
	if tabletCursor != phoneCursor {
		t.Fatalf("cursors diverged: tablet %d, phone %d", tabletCursor, phoneCursor)
	}

	tabletByID, phoneByID := changesByID(tabletState), changesByID(phoneState)
	if len(tabletByID) != 3 {
		t.Fatalf("converged state holds %d entities, want 3", len(tabletByID))
	}
	for id, tabletChange := range tabletByID {
		if fmt.Sprint(tabletChange) != fmt.Sprint(phoneByID[id]) {
			t.Fatalf("device state diverged for %s:\n tablet %v\n phone  %v", id, tabletChange, phoneByID[id])
		}
	}
	if tabletByID["page-1"]["deletedAt"] == nil {
		t.Fatal("the tombstone did not reach both devices")
	}
}

// TestConcurrentPushesAllocateEverySequenceExactlyOnce is the other half of S2's exit criterion.
//
// A reader pulls from its own cursor throughout, which is what makes this a test of SD2 rather than
// of the counter. If a push could commit after a later one — a bare bigserial, say — the reader
// would step over the earlier change and never ask for it again.
func TestConcurrentPushesAllocateEverySequenceExactlyOnce(t *testing.T) {
	const concurrentPushes = 12

	fixture := newSyncFixture(t)
	writers := []*deviceClient{fixture.registerDevice("writer-a"), fixture.registerDevice("writer-b")}
	reader := fixture.registerDevice("reader")

	stopReading := make(chan struct{})
	readerSaw := make(chan []string, 1)
	go func() {
		seen := make([]string, 0, concurrentPushes)
		cursor := int64(0)
		drain := func() bool {
			page, status, err := reader.tryPull(cursor, 100)
			if err != nil || status != 200 {
				t.Errorf("reader pull failed: status %d, error %v", status, err)
				return false
			}
			for _, raw := range page.Changes {
				var change struct {
					ID string `json:"id"`
				}
				if err := json.Unmarshal(raw, &change); err != nil {
					t.Errorf("reader could not decode a change: %v", err)
					return false
				}
				seen = append(seen, change.ID)
			}
			cursor = page.Cursor
			return true
		}

		for {
			select {
			case <-stopReading:
				// Drain to the end, so the assertion is about what the reader was able to see
				// rather than about how its polling happened to line up with the writers.
				for {
					countBefore := len(seen)
					if !drain() || len(seen) == countBefore {
						break
					}
				}
				readerSaw <- seen
				return
			default:
				if !drain() {
					readerSaw <- seen
					return
				}
			}
		}
	}()

	cursors := make([]int64, concurrentPushes)
	var pushers sync.WaitGroup
	for index := range concurrentPushes {
		pushers.Add(1)
		go func() {
			defer pushers.Done()
			writer := writers[index%len(writers)]
			result, status, err := writer.tryPush(newBatchID(t), notebookChange(fmt.Sprintf("notebook-%02d", index), 0, "Concurrent"))
			if err != nil || status != 200 {
				t.Errorf("concurrent push %d failed: status %d, error %v", index, status, err)
				return
			}
			if len(result.Applied) != 1 {
				t.Errorf("concurrent push %d applied %d entities, want 1", index, len(result.Applied))
				return
			}
			cursors[index] = result.Cursor
		}()
	}
	pushers.Wait()
	close(stopReading)

	// Every push allocated its own sequence value, and the values are 1..N with no gap: a gap would
	// mean a value was allocated and lost, and a repeat would mean two pushes shared one.
	allocated := make(map[int64]int, concurrentPushes)
	for index, cursor := range cursors {
		if cursor < 1 || cursor > concurrentPushes {
			t.Fatalf("push %d received cursor %d, outside 1..%d", index, cursor, concurrentPushes)
		}
		allocated[cursor]++
	}
	for sequenceValue := int64(1); sequenceValue <= concurrentPushes; sequenceValue++ {
		if allocated[sequenceValue] != 1 {
			t.Fatalf("sequence value %d was allocated %d times, want exactly once", sequenceValue, allocated[sequenceValue])
		}
	}

	seen := <-readerSaw
	seenOnce := make(map[string]int, len(seen))
	for _, id := range seen {
		seenOnce[id]++
	}
	for index := range concurrentPushes {
		id := fmt.Sprintf("notebook-%02d", index)
		switch seenOnce[id] {
		case 1:
		case 0:
			t.Fatalf("a reader advancing its cursor throughout never saw %s: a committed change was stepped over", id)
		default:
			t.Fatalf("%s was delivered %d times to a strictly advancing cursor", id, seenOnce[id])
		}
	}

	final, cursor := reader.pullEverything(0, 0)
	if len(final) != concurrentPushes || cursor != concurrentPushes {
		t.Fatalf("full pull returned %d changes at cursor %d, want %d and %d", len(final), cursor, concurrentPushes, concurrentPushes)
	}
}

// TestAVersionConflictCarriesTheStoredRow proves a losing writer gets everything it needs to
// resolve the conflict without a second round trip.
func TestAVersionConflictCarriesTheStoredRow(t *testing.T) {
	fixture := newSyncFixture(t)
	tablet := fixture.registerDevice("tablet")
	phone := fixture.registerDevice("phone")

	tablet.push(notebookChange("notebook-1", 0, "Work"))
	tablet.push(notebookChange("notebook-1", 1, "Work and life"))

	// The phone still believes it is editing version 1.
	losing := phone.push(notebookChange("notebook-1", 1, "Personal"))
	if len(losing.Applied) != 0 || len(losing.Rejected) != 1 {
		t.Fatalf("stale push applied %d rejected %d, want 0 and 1", len(losing.Applied), len(losing.Rejected))
	}
	rejection := losing.Rejected[0]
	if rejection.Reason != syncengine.ReasonVersionConflict {
		t.Fatalf("rejection reason = %q, want %q", rejection.Reason, syncengine.ReasonVersionConflict)
	}
	if rejection.Current == nil {
		t.Fatal("a version conflict arrived without the current server row")
	}
	current := decodeChangeObject(t, rejection.Current)
	if current["name"] != "Work and life" || current["version"].(float64) != 2 {
		t.Fatalf("conflict carried %+v, want the stored row at version 2", current)
	}

	// A losing push must not have burned a sequence value.
	if cursor := phone.cursor(); cursor != 2 {
		t.Fatalf("cursor after a fully rejected push = %d, want 2", cursor)
	}

	// Rebasing on what the rejection carried succeeds.
	rebased := phone.push(notebookChange("notebook-1", 2, "Personal"))
	if len(rebased.Applied) != 1 || rebased.Applied[0].Version != 3 {
		t.Fatalf("rebased push = %+v, want version 3", rebased)
	}
}

// TestAnEntityEditedOnlyLocallyIsRefusedRatherThanResurrected covers the other version-conflict
// shape: a client editing something this server has never held.
func TestAnEntityEditedOnlyLocallyIsRefusedRatherThanResurrected(t *testing.T) {
	fixture := newSyncFixture(t)
	tablet := fixture.registerDevice("tablet")

	result := tablet.push(notebookChange("notebook-1", 7, "Restored from somewhere else"))
	if len(result.Rejected) != 1 || result.Rejected[0].Reason != syncengine.ReasonVersionConflict {
		t.Fatalf("push of an unknown entity = %+v, want a version conflict", result)
	}
	if result.Rejected[0].Current != nil {
		t.Fatalf("rejection carried a current row for an entity that does not exist: %s", result.Rejected[0].Current)
	}
	if cursor := tablet.cursor(); cursor != 0 {
		t.Fatalf("cursor = %d after a fully rejected push, want 0", cursor)
	}
}

// TestAMissingParentRejectsOnlyItsOwnSubtree proves one unknown parent does not cost the batch.
func TestAMissingParentRejectsOnlyItsOwnSubtree(t *testing.T) {
	fixture := newSyncFixture(t)
	tablet := fixture.registerDevice("tablet")

	result := tablet.push(
		notebookChange("notebook-1", 0, "Work"),
		sectionChange("section-1", 0, "notebook-missing", "Orphan"),
		pageChange("page-1", 0, "section-1", "Child of an orphan"),
	)
	if len(result.Applied) != 1 || result.Applied[0].ID != "notebook-1" {
		t.Fatalf("applied = %+v, want only the notebook", result.Applied)
	}
	if len(result.Rejected) != 2 {
		t.Fatalf("rejected = %+v, want the section and its page", result.Rejected)
	}
	for _, rejection := range result.Rejected {
		if rejection.Reason != syncengine.ReasonMissingParent {
			t.Fatalf("%s %s rejected as %q, want %q", rejection.Kind, rejection.ID, rejection.Reason, syncengine.ReasonMissingParent)
		}
	}
	if result.Cursor != 1 {
		t.Fatalf("cursor = %d, want 1: the notebook that did apply allocates one sequence value", result.Cursor)
	}

	// The retry the client would make once it notices the parent is wrong.
	retried := tablet.push(
		sectionChange("section-1", 0, "notebook-1", "Meetings"),
		pageChange("page-1", 0, "section-1", "Monday"),
	)
	if len(retried.Applied) != 2 || len(retried.Rejected) != 0 {
		t.Fatalf("retry applied %d rejected %d, want 2 and 0: %+v", len(retried.Applied), len(retried.Rejected), retried.Rejected)
	}
}

// TestUnrecognisedFieldsSurviveARoundTrip is SD5: a field a newer client ships before the server
// knows about it comes back unchanged, and cannot impersonate one the server assigns.
func TestUnrecognisedFieldsSurviveARoundTrip(t *testing.T) {
	fixture := newSyncFixture(t)
	tablet := fixture.registerDevice("tablet")
	phone := fixture.registerDevice("phone")

	fromANewerClient := notebookChange("notebook-1", 0, "Work")
	fromANewerClient["icon"] = "star"
	fromANewerClient["shelf"] = map[string]any{"row": 3, "pinned": true}

	// A field name the protocol owns must not be storable, whatever a client sends.
	fromANewerClient["version"] = 999
	fromANewerClient["seq"] = 999

	tablet.push(fromANewerClient)

	pulled, _ := phone.pullEverything(0, 0)
	if len(pulled) != 1 {
		t.Fatalf("pulled %d changes, want 1", len(pulled))
	}
	notebook := pulled[0]

	if notebook["icon"] != "star" {
		t.Fatalf("unrecognised field did not survive: %+v", notebook)
	}
	shelf, ok := notebook["shelf"].(map[string]any)
	if !ok || shelf["row"].(float64) != 3 || shelf["pinned"] != true {
		t.Fatalf("nested unrecognised field did not survive: %+v", notebook["shelf"])
	}
	if notebook["version"].(float64) != 1 || notebook["seq"].(float64) != 1 {
		t.Fatalf("a client-supplied version or seq overwrote the server's: %+v", notebook)
	}
	if notebook["name"] != "Work" {
		t.Fatalf("a known field went missing: %+v", notebook)
	}
}

// TestPullStopsOnSequenceBoundaries proves a page never splits a push.
//
// A cursor inside a sequence value would let a client acknowledge half a batch — the half that
// contains a page but not the section it moved into — and never be offered the rest.
func TestPullStopsOnSequenceBoundaries(t *testing.T) {
	fixture := newSyncFixture(t)
	tablet := fixture.registerDevice("tablet")
	phone := fixture.registerDevice("phone")

	tablet.push(
		notebookChange("notebook-1", 0, "One"),
		notebookChange("notebook-2", 0, "Two"),
		notebookChange("notebook-3", 0, "Three"),
	)
	tablet.push(
		notebookChange("notebook-4", 0, "Four"),
		notebookChange("notebook-5", 0, "Five"),
		notebookChange("notebook-6", 0, "Six"),
	)
	tablet.push(notebookChange("notebook-7", 0, "Seven"))

	// A limit smaller than the batch still returns the whole batch: `limit` is a request, not a cap,
	// or a client that asked for two could never get past a push of three.
	firstPage := phone.pull(0, 2)
	assertWholeSequence(t, firstPage, 1, 3, true)

	// A limit that falls between two batches cuts at the boundary rather than mid-batch.
	secondPage := phone.pull(firstPage.Cursor, 3)
	assertWholeSequence(t, secondPage, 2, 3, true)

	lastPage := phone.pull(secondPage.Cursor, 3)
	assertWholeSequence(t, lastPage, 3, 1, false)

	if _, cursor := phone.pullEverything(0, 2); cursor != 3 {
		t.Fatalf("paging to the end left the cursor at %d, want 3", cursor)
	}
}

func assertWholeSequence(t *testing.T, page syncengine.PullResult, wantSeq int64, wantChanges int, wantMore bool) {
	t.Helper()
	if len(page.Changes) != wantChanges {
		t.Fatalf("page holds %d changes, want %d", len(page.Changes), wantChanges)
	}
	if page.Cursor != wantSeq || page.HasMore != wantMore {
		t.Fatalf("page cursor %d hasMore %v, want %d and %v", page.Cursor, page.HasMore, wantSeq, wantMore)
	}
	for _, raw := range page.Changes {
		change := decodeChangeObject(t, raw)
		if int64(change["seq"].(float64)) != wantSeq {
			t.Fatalf("page mixes sequence values: %v is at seq %v, want %d", change["id"], change["seq"], wantSeq)
		}
	}
}

// TestRetryingABatchReplaysItsAnswer covers the push whose response was lost.
func TestRetryingABatchReplaysItsAnswer(t *testing.T) {
	fixture := newSyncFixture(t)
	tablet := fixture.registerDevice("tablet")

	batchID := newBatchID(t)
	change := notebookChange("notebook-1", 0, "Work")

	first := tablet.pushBatch(batchID, change)
	if len(first.Applied) != 1 || first.Applied[0].Version != 1 || first.Cursor != 1 {
		t.Fatalf("first attempt = %+v", first)
	}

	// The same batch id, as a client that never saw the answer would resend it.
	replayed := tablet.pushBatch(batchID, change)
	if fmt.Sprint(replayed) != fmt.Sprint(first) {
		t.Fatalf("replay returned %+v, want the stored answer %+v", replayed, first)
	}
	if cursor := tablet.cursor(); cursor != 1 {
		t.Fatalf("cursor = %d after a replayed batch, want 1: the retry burned a sequence value", cursor)
	}

	pulled, _ := tablet.pullEverything(0, 0)
	if len(pulled) != 1 || pulled[0]["version"].(float64) != 1 {
		t.Fatalf("the retry applied the work twice: %+v", pulled)
	}

	// A different batch id is a different push, and this one is genuinely stale.
	conflicting := tablet.push(change)
	if len(conflicting.Rejected) != 1 || conflicting.Rejected[0].Reason != syncengine.ReasonVersionConflict {
		t.Fatalf("re-pushing under a new batch id = %+v, want a version conflict", conflicting)
	}
}

// TestChangesAreScopedToOneAccount is the tenancy check. It is in the integration suite rather than
// beside the handler because the boundary is enforced by every WHERE clause, not by one function.
func TestChangesAreScopedToOneAccount(t *testing.T) {
	owner := newSyncFixture(t)
	stranger := newSyncFixture(t)

	ownerDevice := owner.registerDevice("owner")
	strangerDevice := stranger.registerDevice("stranger")

	ownerDevice.push(notebookChange("notebook-1", 0, "Private"))

	if cursor := strangerDevice.cursor(); cursor != 0 {
		t.Fatalf("another account's push moved this account's cursor to %d", cursor)
	}
	if delta := strangerDevice.pull(0, 0); len(delta.Changes) != 0 {
		t.Fatalf("another account's changes are visible: %s", delta.Changes)
	}

	// Same entity id, different account: the primary key is (account_id, id), so this is a create
	// rather than a conflict.
	created := strangerDevice.push(notebookChange("notebook-1", 0, "Also private"))
	if len(created.Applied) != 1 || created.Applied[0].Version != 1 {
		t.Fatalf("an id another account uses was refused: %+v", created)
	}
}

// TestMalformedChangesAreRefusedIndividually proves one bad entity cannot poison a batch.
//
// The NUL case is the one that matters most: PostgreSQL `text` cannot store it, so letting it reach
// the driver would fail the transaction rather than the entity, and the client would retry the same
// batch for ever.
func TestMalformedChangesAreRefusedIndividually(t *testing.T) {
	fixture := newSyncFixture(t)
	tablet := fixture.registerDevice("tablet")

	nullByteInName := notebookChange("notebook-bad", 0, "before\x00after")
	overLongName := notebookChange("notebook-long", 0, strings.Repeat("a", 513))
	unknownKind := map[string]any{"kind": "page_content", "id": "page-1", "baseVersion": 0}
	emptyID := notebookChange("", 0, "No id")

	result := tablet.push(
		notebookChange("notebook-good", 0, "Fine"),
		nullByteInName,
		overLongName,
		unknownKind,
		emptyID,
	)

	if len(result.Applied) != 1 || result.Applied[0].ID != "notebook-good" {
		t.Fatalf("applied = %+v, want only the valid notebook", result.Applied)
	}
	rejectionsByID := make(map[string]syncengine.RejectedChange, len(result.Rejected))
	for _, rejection := range result.Rejected {
		rejectionsByID[rejection.ID] = rejection
	}
	if reason := rejectionsByID["notebook-bad"].Reason; reason != syncengine.ReasonMalformed {
		t.Fatalf("a NUL byte was rejected as %q, want %q", reason, syncengine.ReasonMalformed)
	}
	if reason := rejectionsByID["notebook-long"].Reason; reason != syncengine.ReasonTooLarge {
		t.Fatalf("an over-long name was rejected as %q, want %q", reason, syncengine.ReasonTooLarge)
	}
	if reason := rejectionsByID["page-1"].Reason; reason != syncengine.ReasonMalformed {
		t.Fatalf("an unknown kind was rejected as %q, want %q", reason, syncengine.ReasonMalformed)
	}
	if reason := rejectionsByID[""].Reason; reason != syncengine.ReasonMalformed {
		t.Fatalf("an empty id was rejected as %q, want %q", reason, syncengine.ReasonMalformed)
	}

	// The good entity is stored and the batch still allocated exactly one sequence value.
	if result.Cursor != 1 {
		t.Fatalf("cursor = %d, want 1", result.Cursor)
	}
	pulled, _ := tablet.pullEverything(0, 0)
	if len(pulled) != 1 || pulled[0]["id"] != "notebook-good" {
		t.Fatalf("stored state = %+v, want only the valid notebook", pulled)
	}
}

// TestTheSameEntityTwiceInOneBatchIsRefused: the second copy would be written against a version the
// first had already moved, so it is refused rather than silently overwriting.
func TestTheSameEntityTwiceInOneBatchIsRefused(t *testing.T) {
	fixture := newSyncFixture(t)
	tablet := fixture.registerDevice("tablet")

	result := tablet.push(
		notebookChange("notebook-1", 0, "First"),
		notebookChange("notebook-1", 0, "Second"),
	)
	if len(result.Applied) != 1 || len(result.Rejected) != 1 {
		t.Fatalf("applied %d rejected %d, want 1 and 1", len(result.Applied), len(result.Rejected))
	}
	if result.Rejected[0].Reason != syncengine.ReasonMalformed {
		t.Fatalf("the duplicate was rejected as %q, want %q", result.Rejected[0].Reason, syncengine.ReasonMalformed)
	}

	pulled, _ := tablet.pullEverything(0, 0)
	if pulled[0]["name"] != "First" {
		t.Fatalf("stored name = %v, want the first copy", pulled[0]["name"])
	}
}

// TestAnEmptyPushIsCheapAndHarmless: a client with an empty outbox that pushes anyway must not move
// anyone's cursor.
func TestAnEmptyPushIsCheapAndHarmless(t *testing.T) {
	fixture := newSyncFixture(t)
	tablet := fixture.registerDevice("tablet")

	tablet.push(notebookChange("notebook-1", 0, "Work"))
	before := tablet.cursor()

	empty := tablet.push()
	if len(empty.Applied) != 0 || len(empty.Rejected) != 0 {
		t.Fatalf("empty push = %+v", empty)
	}
	if empty.Cursor != before {
		t.Fatalf("empty push moved the cursor from %d to %d", before, empty.Cursor)
	}
}

// TestADocumentBodyRoundTripsThroughTheServer is S3's first exit criterion: a page's document
// reaches another device byte for byte, through a server that never looks inside it.
func TestADocumentBodyRoundTripsThroughTheServer(t *testing.T) {
	fixture := newSyncFixture(t)
	tablet := fixture.registerDevice("tablet")
	phone := fixture.registerDevice("phone")

	// Bytes that are not text, so a round trip proves base64 rather than a string copy: a NUL, a
	// 0xff no UTF-8 decoder would accept, and a newline where PostgreSQL's own base64 puts one.
	doc := append([]byte("{\"schema\":3,\"o\":\"hello\"}\x00\xff\n"), make([]byte, 200)...)

	pushed := tablet.push(
		notebookChange("notebook-1", 0, "Work"),
		sectionChange("section-1", 0, "notebook-1", "Meetings"),
		pageChange("page-1", 0, "section-1", "Monday"),
		pageContentChange("page-1", 0, doc),
	)
	if len(pushed.Applied) != 4 || len(pushed.Rejected) != 0 {
		t.Fatalf("push applied %d rejected %+v, want 4 and none", len(pushed.Applied), pushed.Rejected)
	}

	changes, _ := phone.pullEverything(0, 0)
	kinds := make([]string, 0, len(changes))
	for _, change := range changes {
		kinds = append(kinds, change["kind"].(string))
	}
	if fmt.Sprint(kinds) != "[notebook section page pageContent]" {
		t.Fatalf("pull order = %v, want a body after the page it hangs from", kinds)
	}

	body := changeOfKind(t, changes, "pageContent")
	returned, err := base64.StdEncoding.DecodeString(body["doc"].(string))
	if err != nil {
		t.Fatalf("doc did not decode as base64: %v", err)
	}
	if !bytes.Equal(returned, doc) {
		t.Fatalf("doc came back as %d bytes, want the %d that were sent", len(returned), len(doc))
	}

	digest := sha256.Sum256(doc)
	if body["docSha256"] != base64.StdEncoding.EncodeToString(digest[:]) {
		t.Fatalf("docSha256 = %v, want the digest of what was stored", body["docSha256"])
	}
	if body["pageId"] != "page-1" || body["format"] != "json/1" {
		t.Fatalf("body = %+v, want it addressed to page-1 in json/1", body)
	}
	// Absent on the push, so the server supplies what a client that has never heard of encodings
	// means by leaving it out.
	if body["enc"] != "none/1" {
		t.Fatalf("enc = %v, want the default", body["enc"])
	}
}

// TestABodyThatDoesNotMatchItsDigestIsRefused: a truncated document must be a rejected entity, not
// a document that decodes to nothing on every device that pulls it.
func TestABodyThatDoesNotMatchItsDigestIsRefused(t *testing.T) {
	fixture := newSyncFixture(t)
	tablet := fixture.registerDevice("tablet")

	tablet.push(
		notebookChange("notebook-1", 0, "Work"),
		sectionChange("section-1", 0, "notebook-1", "Meetings"),
		pageChange("page-1", 0, "section-1", "Monday"),
	)

	lying := pageContentChange("page-1", 0, []byte("the body that was actually sent"))
	claimed := sha256.Sum256([]byte("a different body entirely"))
	lying["docSha256"] = claimed[:]

	// Pushed with a page rename beside it, so the test also shows the rejection is per entity.
	result := tablet.push(lying, pageChange("page-1", 1, "section-1", "Tuesday"))
	if len(result.Rejected) != 1 || result.Rejected[0].Reason != syncengine.ReasonMalformed {
		t.Fatalf("rejected = %+v, want one malformed body", result.Rejected)
	}
	if len(result.Applied) != 1 || result.Applied[0].Kind != "page" {
		t.Fatalf("applied = %+v, want the page beside it to land", result.Applied)
	}
}

// TestADocumentLargerThanTheCapIsRefused holds the line the batch cap cannot: a body is bounded on
// its own, before base64 turns 2 MiB into nearly 3 MB of request.
func TestADocumentLargerThanTheCapIsRefused(t *testing.T) {
	fixture := newSyncFixture(t)
	tablet := fixture.registerDevice("tablet")

	tablet.push(
		notebookChange("notebook-1", 0, "Work"),
		sectionChange("section-1", 0, "notebook-1", "Meetings"),
		pageChange("page-1", 0, "section-1", "Monday"),
	)

	huge := pageContentChange("page-1", 0, make([]byte, (2<<20)+1))
	result := tablet.push(huge)
	if len(result.Rejected) != 1 || result.Rejected[0].Reason != syncengine.ReasonTooLarge {
		t.Fatalf("rejected = %+v, want one too_large body", result.Rejected)
	}
}

// TestABodyWithoutItsPageIsRejectedNotStored: `page_content` hangs off `pages` with a real foreign
// key, so the parent check is what keeps a client bug from being a failed transaction.
func TestABodyWithoutItsPageIsRejectedNotStored(t *testing.T) {
	fixture := newSyncFixture(t)
	tablet := fixture.registerDevice("tablet")

	result := tablet.push(pageContentChange("page-that-does-not-exist", 0, []byte("{}")))
	if len(result.Rejected) != 1 || result.Rejected[0].Reason != syncengine.ReasonMissingParent {
		t.Fatalf("rejected = %+v, want one missing_parent", result.Rejected)
	}
	if result.Cursor != 0 {
		t.Fatalf("cursor = %d, want a batch that changed nothing to allocate nothing", result.Cursor)
	}
}

// TestADeviceCanBeRenamedAndARevokedOneCannot: the name a client registers with is all it knew
// about itself, and two devices of the same model report the same thing. Renaming is how a person
// tells the rows apart before deciding which to revoke.
func TestADeviceCanBeRenamedAndARevokedOneCannot(t *testing.T) {
	fixture := newSyncFixture(t)
	tablet := fixture.registerDevice("Pixel Tablet")
	other := fixture.registerDevice("Pixel Tablet")

	status := fixture.request(http.MethodPatch, "/v1/devices/"+other.deviceID, tablet.token,
		map[string]any{"name": "Studio tablet"}, nil)
	if status != http.StatusNoContent {
		t.Fatalf("rename status = %d, want 204", status)
	}

	var listed struct {
		Devices []struct {
			DeviceID string `json:"deviceId"`
			Name     string `json:"name"`
		} `json:"devices"`
	}
	fixture.request(http.MethodGet, "/v1/devices", tablet.token, nil, &listed)
	renamed := false
	for _, device := range listed.Devices {
		if device.DeviceID == other.deviceID && device.Name == "Studio tablet" {
			renamed = true
		}
	}
	if !renamed {
		t.Fatalf("device list = %+v, want the renamed device", listed.Devices)
	}

	// An empty name would leave a row nothing can be said about.
	if status := fixture.request(http.MethodPatch, "/v1/devices/"+other.deviceID, tablet.token,
		map[string]any{"name": "   "}, nil); status != http.StatusBadRequest {
		t.Fatalf("blank rename status = %d, want 400", status)
	}

	// A revoked row is history. Relabelling it would make the log of what happened disagree with
	// what is written next to it.
	if status := fixture.request(http.MethodDelete, "/v1/devices/"+other.deviceID, tablet.token, nil, nil); status != http.StatusNoContent {
		t.Fatalf("revoke status = %d, want 204", status)
	}
	if status := fixture.request(http.MethodPatch, "/v1/devices/"+other.deviceID, tablet.token,
		map[string]any{"name": "Renamed after the fact"}, nil); status != http.StatusNotFound {
		t.Fatalf("renaming a revoked device = %d, want 404", status)
	}
}
