package tests

// S5: attachment blobs. The exit criterion in syncPlan.md §9 is one sentence with two halves —
// "the same image on two devices uploads once; deleting its last page frees it" — and the first two
// tests here are those halves. The rest hold the invariant that makes them safe: the server never
// accepts a reference to bytes it does not have, and never removes bytes something still points at.

import (
	"bytes"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/AquilaIgnis/viveCServer/internal/httpapi"
	"github.com/AquilaIgnis/viveCServer/internal/syncengine"
)

// pictureBytes stands in for a re-encoded import. Not text, so a round trip proves bytes rather
// than a string copy.
func pictureBytes(seed byte, size int) []byte {
	content := make([]byte, size)
	for index := range content {
		content[index] = seed ^ byte(index*7)
	}
	return content
}

// TestTheSamePictureFromTwoDevicesIsStoredOnce is the first half of S5's exit criterion.
//
// Deduplication is asserted on the filesystem rather than on the status code, because the status
// code is what we would have to get right anyway and the file count is the thing the criterion is
// actually about.
func TestTheSamePictureFromTwoDevicesIsStoredOnce(t *testing.T) {
	fixture := newSyncFixture(t)
	tablet := fixture.registerDevice("tablet")
	phone := fixture.registerDevice("phone")

	picture := pictureBytes(0x5a, 64*1024)
	digest := digestOf(picture)

	if presence := tablet.blobPresence(digest); presence.status != http.StatusNotFound {
		t.Fatalf("presence before upload = %d, want 404", presence.status)
	}

	first := tablet.uploadBlob(digest, picture)
	if first.status != http.StatusCreated {
		t.Fatalf("first upload = %d, want 201", first.status)
	}
	if got := first.header.Get("ETag"); got != `"`+digest+`"` {
		t.Fatalf("ETag = %q, want the digest", got)
	}

	// The second device asks first, which is what a real client does and what makes the dedup free.
	if presence := phone.blobPresence(digest); presence.status != http.StatusNoContent {
		t.Fatalf("presence after upload = %d, want 204", presence.status)
	}

	second := phone.uploadBlob(digest, picture)
	if second.status != http.StatusNoContent {
		t.Fatalf("second upload = %d, want 204 already present", second.status)
	}

	if stored := fixture.storedBlobFiles(); stored != 1 {
		t.Fatalf("the store holds %d files, want the same picture stored once", stored)
	}

	downloaded := phone.downloadBlob(digest)
	if downloaded.status != http.StatusOK {
		t.Fatalf("download = %d, want 200", downloaded.status)
	}
	if !bytes.Equal(downloaded.body, picture) {
		t.Fatalf("downloaded %d bytes, want the %d that were uploaded", len(downloaded.body), len(picture))
	}
}

// TestDeletingTheLastPageFreesThePicture is the second half of S5's exit criterion.
//
// It runs the whole chain: upload, reference, delete, sweep. The retention is zero so the test does
// not wait a day for what a server does on an interval.
func TestDeletingTheLastPageFreesThePicture(t *testing.T) {
	fixture := newSyncFixture(t)
	tablet := fixture.registerDevice("tablet")

	picture := pictureBytes(0x11, 8*1024)
	digest := digestOf(picture)

	if uploaded := tablet.uploadBlob(digest, picture); uploaded.status != http.StatusCreated {
		t.Fatalf("upload = %d, want 201", uploaded.status)
	}

	pushed := tablet.push(
		notebookChange("notebook-1", 0, "Work"),
		sectionChange("section-1", 0, "notebook-1", "Meetings"),
		pageChange("page-1", 0, "section-1", "Monday"),
		pageContentChangeWithBlobs("page-1", 0, []byte(`{"schema":3}`), digest),
		attachmentChange(digest, 0, int64(len(picture))),
	)
	if len(pushed.Rejected) != 0 {
		t.Fatalf("push rejected %+v, want none", pushed.Rejected)
	}

	// A referenced picture survives a sweep, which is the half of this that must never break.
	fixture.sweepAttachments(0)
	if stored := fixture.storedBlobFiles(); stored != 1 {
		t.Fatalf("a referenced picture was swept: the store holds %d files", stored)
	}
	if presence := tablet.blobPresence(digest); presence.status != http.StatusNoContent {
		t.Fatalf("presence after a sweep = %d, want the picture still there", presence.status)
	}

	// The client deletes the page and, its own reference count having reached zero, the attachment
	// row with it. Both are ordinary tombstones pushed the ordinary way.
	deletedPage := pageChange("page-1", 1, "section-1", "Monday")
	deletedPage["deletedAt"] = 1_700_000_100_000
	deletedAttachment := attachmentChange(digest, 1, int64(len(picture)))
	deletedAttachment["deletedAt"] = 1_700_000_100_000

	deleted := tablet.push(deletedPage, deletedAttachment)
	if len(deleted.Rejected) != 0 {
		t.Fatalf("deleting rejected %+v, want none", deleted.Rejected)
	}

	fixture.sweepAttachments(0)

	if stored := fixture.storedBlobFiles(); stored != 0 {
		t.Fatalf("the store still holds %d files after the last page holding them was deleted", stored)
	}
	if presence := tablet.blobPresence(digest); presence.status != http.StatusNotFound {
		t.Fatalf("presence after the sweep = %d, want 404", presence.status)
	}
	if storage := fixture.accountStorageBytes(); storage != 0 {
		t.Fatalf("account storage = %d, want it refunded to 0", storage)
	}
}

// TestADocumentCannotReferenceAnAttachmentTheServerDoesNotHold is SD7's invariant, and the reason
// the sweeper is allowed to exist at all: reachability is only knowable because the reference set is
// refused unless it is satisfiable.
func TestADocumentCannotReferenceAnAttachmentTheServerDoesNotHold(t *testing.T) {
	fixture := newSyncFixture(t)
	tablet := fixture.registerDevice("tablet")

	picture := pictureBytes(0x22, 4096)
	digest := digestOf(picture)

	result := tablet.push(
		notebookChange("notebook-1", 0, "Work"),
		sectionChange("section-1", 0, "notebook-1", "Meetings"),
		pageChange("page-1", 0, "section-1", "Monday"),
		pageContentChangeWithBlobs("page-1", 0, []byte(`{"schema":3}`), digest),
	)
	if len(result.Applied) != 3 {
		t.Fatalf("applied %d, want the three rows that do not need bytes", len(result.Applied))
	}
	if len(result.Rejected) != 1 || result.Rejected[0].Reason != syncengine.ReasonMissingBlob {
		t.Fatalf("rejected %+v, want one missing_blob", result.Rejected)
	}
	if !strings.Contains(result.Rejected[0].Message, digest) {
		// The client's next move is to upload exactly these; a message that did not name them would
		// make it diff its own reference set to find out which.
		t.Fatalf("the rejection message %q does not name the missing digest", result.Rejected[0].Message)
	}

	// The client uploads and pushes again, which is the whole recovery path and needs no other
	// server support.
	if uploaded := tablet.uploadBlob(digest, picture); uploaded.status != http.StatusCreated {
		t.Fatalf("upload = %d, want 201", uploaded.status)
	}
	retried := tablet.push(pageContentChangeWithBlobs("page-1", 0, []byte(`{"schema":3}`), digest))
	if len(retried.Applied) != 1 || len(retried.Rejected) != 0 {
		t.Fatalf("the retry applied %d rejected %+v, want it to land", len(retried.Applied), retried.Rejected)
	}

	// And the reference set comes back on a pull, so the other device knows what to fetch.
	phone := fixture.registerDevice("phone")
	changes, _ := phone.pullEverything(0, 0)
	body := changeOfKind(t, changes, "pageContent")
	refs, isList := body["blobRefs"].([]any)
	if !isList || len(refs) != 1 || refs[0] != digest {
		t.Fatalf("blobRefs = %v, want the one digest that was pushed", body["blobRefs"])
	}
}

// TestAnAttachmentRowCannotOutrunItsBytes: the same rule for the metadata kind, whose id is itself
// the digest.
func TestAnAttachmentRowCannotOutrunItsBytes(t *testing.T) {
	fixture := newSyncFixture(t)
	tablet := fixture.registerDevice("tablet")

	picture := pictureBytes(0x33, 2048)
	digest := digestOf(picture)

	missing := tablet.push(attachmentChange(digest, 0, int64(len(picture))))
	if len(missing.Rejected) != 1 || missing.Rejected[0].Reason != syncengine.ReasonMissingBlob {
		t.Fatalf("rejected %+v, want one missing_blob", missing.Rejected)
	}

	tablet.uploadBlob(digest, picture)

	// A row that disagrees with the bytes it names is refused, the same way a document body that
	// disagrees with its digest is.
	lying := tablet.push(attachmentChange(digest, 0, int64(len(picture))+1))
	if len(lying.Rejected) != 1 || lying.Rejected[0].Reason != syncengine.ReasonMalformed {
		t.Fatalf("rejected %+v, want the wrong byteCount refused as malformed", lying.Rejected)
	}

	honest := tablet.push(attachmentChange(digest, 0, int64(len(picture))))
	if len(honest.Applied) != 1 || len(honest.Rejected) != 0 {
		t.Fatalf("applied %d rejected %+v, want the honest row to land", len(honest.Applied), honest.Rejected)
	}

	// An id that cannot be a digest is a shape failure, not a missing blob: there is no upload that
	// would ever make it storable.
	notADigest := attachmentChange("attachment-1", 0, 10)
	shaped := tablet.push(notADigest)
	if len(shaped.Rejected) != 1 || shaped.Rejected[0].Reason != syncengine.ReasonMalformed {
		t.Fatalf("rejected %+v, want an id that is not a digest refused as malformed", shaped.Rejected)
	}
}

// TestATombstoneCanNameAnAttachmentThatIsAlreadyGone: the one case the presence rule must not
// apply to. Holding a delete to it would leave a row the client can never retract, on every device,
// for ever.
func TestATombstoneCanNameAnAttachmentThatIsAlreadyGone(t *testing.T) {
	fixture := newSyncFixture(t)
	tablet := fixture.registerDevice("tablet")

	picture := pictureBytes(0x44, 1024)
	digest := digestOf(picture)

	tablet.uploadBlob(digest, picture)
	tablet.push(attachmentChange(digest, 0, int64(len(picture))))

	// The bytes go without the row: the client's own storage decided the picture was unreachable
	// before the server's sweep did.
	fixture.sweepAttachments(0)
	if presence := tablet.blobPresence(digest); presence.status != http.StatusNoContent {
		t.Fatalf("a live attachment row did not keep its bytes: presence = %d", presence.status)
	}

	tombstone := attachmentChange(digest, 1, int64(len(picture)))
	tombstone["deletedAt"] = 1_700_000_100_000
	deleted := tablet.push(tombstone)
	if len(deleted.Applied) != 1 || len(deleted.Rejected) != 0 {
		t.Fatalf("applied %d rejected %+v, want the tombstone to land", len(deleted.Applied), deleted.Rejected)
	}

	fixture.sweepAttachments(0)
	if presence := tablet.blobPresence(digest); presence.status != http.StatusNotFound {
		t.Fatalf("presence after the tombstone was swept = %d, want 404", presence.status)
	}
}

// TestAnAccountCannotReadAnotherAccountsAttachment: holding a digest is not holding a blob. The
// database row is the capability, and an account gets one only by uploading the bytes itself.
func TestAnAccountCannotReadAnotherAccountsAttachment(t *testing.T) {
	owner := newSyncFixture(t)
	ownerDevice := owner.registerDevice("owner")

	picture := pictureBytes(0x55, 4096)
	digest := digestOf(picture)
	ownerDevice.uploadBlob(digest, picture)

	stranger := newSyncFixture(t)
	strangerDevice := stranger.registerDevice("stranger")

	if presence := strangerDevice.blobPresence(digest); presence.status != http.StatusNotFound {
		t.Fatalf("a stranger's presence check = %d, want 404", presence.status)
	}
	if download := strangerDevice.downloadBlob(digest); download.status != http.StatusNotFound {
		t.Fatalf("a stranger's download = %d, want 404", download.status)
	}

	// And without a token at all.
	anonymous := &deviceClient{t: owner.t, fixture: owner, name: "anonymous"}
	if download := anonymous.downloadBlob(digest); download.status != http.StatusUnauthorized {
		t.Fatalf("an unauthenticated download = %d, want 401", download.status)
	}
}

// TestBytesThatDoNotMatchTheirDigestAreRefused. A content-addressed store that accepts an unverified
// name is a filesystem with a confusing API: the first truncated upload puts a file in it that every
// device will fetch and fail to decode.
func TestBytesThatDoNotMatchTheirDigestAreRefused(t *testing.T) {
	fixture := newSyncFixture(t)
	tablet := fixture.registerDevice("tablet")

	picture := pictureBytes(0x66, 4096)
	digest := digestOf(picture)

	truncated := tablet.uploadBlob(digest, picture[:2048])
	if truncated.status != http.StatusBadRequest {
		t.Fatalf("a truncated upload = %d, want 400", truncated.status)
	}
	if stored := fixture.storedBlobFiles(); stored != 0 {
		t.Fatalf("a refused upload left %d files behind", stored)
	}
	if presence := tablet.blobPresence(digest); presence.status != http.StatusNotFound {
		t.Fatalf("presence after a refused upload = %d, want 404", presence.status)
	}

	malformedDigest := tablet.uploadBlob("not-a-digest", picture)
	if malformedDigest.status != http.StatusBadRequest {
		t.Fatalf("an upload under a malformed digest = %d, want 400", malformedDigest.status)
	}

	// Uppercase is a different spelling of the same identity, and two spellings of one identity is
	// how a picture becomes two files and two charges against the same account's storage.
	uppercase := tablet.uploadBlob(strings.ToUpper(digest), picture)
	if uppercase.status != http.StatusBadRequest {
		t.Fatalf("an upload under an uppercase digest = %d, want 400", uppercase.status)
	}
}

// TestAnAttachmentIsCacheableAndResumable: what a content-addressed URL buys, and the reason the
// download path goes through ServeContent rather than io.Copy.
func TestAnAttachmentIsCacheableAndResumable(t *testing.T) {
	fixture := newSyncFixture(t)
	tablet := fixture.registerDevice("tablet")

	picture := pictureBytes(0x77, 100_000)
	digest := digestOf(picture)
	tablet.uploadBlob(digest, picture)

	whole := tablet.downloadBlob(digest)
	if whole.status != http.StatusOK || !bytes.Equal(whole.body, picture) {
		t.Fatalf("download = %d with %d bytes", whole.status, len(whole.body))
	}
	if !strings.Contains(whole.header.Get("Cache-Control"), "immutable") {
		t.Fatalf("Cache-Control = %q, want an immutable response", whole.header.Get("Cache-Control"))
	}
	if whole.header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("an attachment is served without nosniff, so a browser may sniff user bytes as HTML")
	}

	// A client that already has it pays a 304 rather than the picture.
	cached := tablet.downloadBlobWithHeaders(digest, map[string]string{"If-None-Match": `"` + digest + `"`})
	if cached.status != http.StatusNotModified {
		t.Fatalf("a conditional download = %d, want 304", cached.status)
	}
	if len(cached.body) != 0 {
		t.Fatalf("a 304 carried %d bytes", len(cached.body))
	}

	// An interrupted transfer resumes instead of restarting.
	resumed := tablet.downloadBlobWithHeaders(digest, map[string]string{"Range": "bytes=90000-"})
	if resumed.status != http.StatusPartialContent {
		t.Fatalf("a ranged download = %d, want 206", resumed.status)
	}
	if !bytes.Equal(resumed.body, picture[90000:]) {
		t.Fatalf("the range returned %d bytes, want the last %d", len(resumed.body), len(picture)-90000)
	}
	if got, want := resumed.header.Get("Content-Range"), fmt.Sprintf("bytes 90000-%d/%d", len(picture)-1, len(picture)); got != want {
		t.Fatalf("Content-Range = %q, want %q", got, want)
	}
}

// TestAnAttachmentLargerThanTheCapIsRefused. The per-attachment cap is the only storage limit this
// server has: there is no account quota, because the operator and the user are the same person and
// the disk is the limit (syncPlan.md §12 decision 1).
func TestAnAttachmentLargerThanTheCapIsRefused(t *testing.T) {
	fixture := newSyncFixtureWithBlobLimits(t, httpapi.BlobLimits{MaxBlobBytes: 8 * 1024})
	tablet := fixture.registerDevice("tablet")

	oversized := pictureBytes(0x88, 8*1024+1)
	if result := tablet.uploadBlob(digestOf(oversized), oversized); result.status != http.StatusRequestEntityTooLarge {
		t.Fatalf("an oversized upload = %d, want 413", result.status)
	}
	if stored := fixture.storedBlobFiles(); stored != 0 {
		t.Fatalf("a refused upload left %d files behind", stored)
	}

	// Nothing stops an account accumulating: two uploads at the cap both land, and both are charged
	// to the figure the admin dashboard reports.
	first := pictureBytes(0x89, 8*1024)
	if result := tablet.uploadBlob(digestOf(first), first); result.status != http.StatusCreated {
		t.Fatalf("the first upload = %d, want 201", result.status)
	}
	second := pictureBytes(0x8a, 8*1024)
	if result := tablet.uploadBlob(digestOf(second), second); result.status != http.StatusCreated {
		t.Fatalf("the second upload = %d, want 201", result.status)
	}
	if storage := fixture.accountStorageBytes(); storage != 16*1024 {
		t.Fatalf("account storage = %d, want both uploads counted", storage)
	}
}

// TestUnreferencedBytesSurviveTheRetentionWindow is the reason the sweep is two-phase.
//
// A client that uploads a picture and then loses connectivity before pushing the change that
// references it must find its bytes still there when it comes back.
func TestUnreferencedBytesSurviveTheRetentionWindow(t *testing.T) {
	fixture := newSyncFixture(t)
	tablet := fixture.registerDevice("tablet")

	picture := pictureBytes(0x99, 4096)
	digest := digestOf(picture)
	tablet.uploadBlob(digest, picture)

	// Marked, but nothing is old enough to delete.
	fixture.sweepAttachments(time.Hour)
	if stored := fixture.storedBlobFiles(); stored != 1 {
		t.Fatalf("the store holds %d files, want the upload kept inside its retention window", stored)
	}
	if presence := tablet.blobPresence(digest); presence.status != http.StatusNoContent {
		t.Fatalf("presence inside the retention window = %d, want the bytes still there", presence.status)
	}

	// The client comes back and pushes what it was uploading for. The mark is cleared rather than
	// remembered, so the retention clock does not keep running under a picture that is now in use.
	tablet.push(
		notebookChange("notebook-1", 0, "Work"),
		sectionChange("section-1", 0, "notebook-1", "Meetings"),
		pageChange("page-1", 0, "section-1", "Monday"),
		pageContentChangeWithBlobs("page-1", 0, []byte(`{"schema":3}`), digest),
	)

	fixture.sweepAttachments(0)
	if presence := tablet.blobPresence(digest); presence.status != http.StatusNoContent {
		t.Fatalf("a picture a live page references was swept: presence = %d", presence.status)
	}
	if stored := fixture.storedBlobFiles(); stored != 1 {
		t.Fatalf("the store holds %d files, want the referenced picture kept", stored)
	}
}

// TestRemovingAPictureFromADocumentReleasesIt: the reference set is replaced by every push, so
// deleting a picture from a page is enough. The page itself stays.
func TestRemovingAPictureFromADocumentReleasesIt(t *testing.T) {
	fixture := newSyncFixture(t)
	tablet := fixture.registerDevice("tablet")

	kept := pictureBytes(0xa1, 2048)
	removed := pictureBytes(0xa2, 2048)
	keptDigest, removedDigest := digestOf(kept), digestOf(removed)

	tablet.uploadBlob(keptDigest, kept)
	tablet.uploadBlob(removedDigest, removed)

	tablet.push(
		notebookChange("notebook-1", 0, "Work"),
		sectionChange("section-1", 0, "notebook-1", "Meetings"),
		pageChange("page-1", 0, "section-1", "Monday"),
		pageContentChangeWithBlobs("page-1", 0, []byte(`{"schema":3}`), keptDigest, removedDigest),
	)

	// The user deletes one picture from the page. The document is pushed again with one reference.
	edited := tablet.push(pageContentChangeWithBlobs("page-1", 1, []byte(`{"schema":3,"edited":true}`), keptDigest))
	if len(edited.Rejected) != 0 {
		t.Fatalf("the edit was rejected %+v", edited.Rejected)
	}

	fixture.sweepAttachments(0)
	if stored := fixture.storedBlobFiles(); stored != 1 {
		t.Fatalf("the store holds %d files, want only the picture still on the page", stored)
	}
	if presence := tablet.blobPresence(removedDigest); presence.status != http.StatusNotFound {
		t.Fatalf("the removed picture is still stored: presence = %d", presence.status)
	}
	if presence := tablet.blobPresence(keptDigest); presence.status != http.StatusNoContent {
		t.Fatalf("the picture still on the page was swept: presence = %d", presence.status)
	}
}

// TestTombstoningADocumentReleasesItsPictures: a deleted body references nothing, whatever it still
// carries, so a page's pictures are freed without waiting for the page tombstone to be purged.
func TestTombstoningADocumentReleasesItsPictures(t *testing.T) {
	fixture := newSyncFixture(t)
	tablet := fixture.registerDevice("tablet")

	picture := pictureBytes(0xb1, 2048)
	digest := digestOf(picture)
	tablet.uploadBlob(digest, picture)

	tablet.push(
		notebookChange("notebook-1", 0, "Work"),
		sectionChange("section-1", 0, "notebook-1", "Meetings"),
		pageChange("page-1", 0, "section-1", "Monday"),
		pageContentChangeWithBlobs("page-1", 0, []byte(`{"schema":3}`), digest),
	)

	if deleted := tablet.push(deletedPageContentChange("page-1", 1)); len(deleted.Rejected) != 0 {
		t.Fatalf("tombstoning the body was rejected %+v", deleted.Rejected)
	}

	fixture.sweepAttachments(0)
	if stored := fixture.storedBlobFiles(); stored != 0 {
		t.Fatalf("the store holds %d files, want the tombstoned body's picture released", stored)
	}
	if presence := tablet.blobPresence(digest); presence.status != http.StatusNotFound {
		t.Fatalf("presence after tombstoning the body = %d, want 404", presence.status)
	}
}

// TestTheSameBytesInTwoAccountsAreOneFileAndTwoClaims. Cross-account dedup costs disk nothing and
// leaks nothing, and the sweeper must not unlink a file another account still holds.
func TestTheSameBytesInTwoAccountsAreOneFileAndTwoClaims(t *testing.T) {
	first := newSyncFixture(t)
	firstDevice := first.registerDevice("first")

	picture := pictureBytes(0xc1, 4096)
	digest := digestOf(picture)
	firstDevice.uploadBlob(digest, picture)

	// A second account on the same server and the same directory, which is what a family
	// self-hosting one instance looks like.
	second := newSyncFixtureOnStore(t, first)
	secondDevice := second.registerDevice("second")

	// It must upload for itself: a presence check is account-scoped, so knowing the digest gives it
	// nothing.
	if presence := secondDevice.blobPresence(digest); presence.status != http.StatusNotFound {
		t.Fatalf("the second account saw the first account's blob: presence = %d", presence.status)
	}
	if uploaded := secondDevice.uploadBlob(digest, picture); uploaded.status != http.StatusCreated {
		t.Fatalf("the second account's upload = %d, want 201", uploaded.status)
	}
	if stored := first.storedBlobFiles(); stored != 1 {
		t.Fatalf("the store holds %d files, want the same bytes stored once for both accounts", stored)
	}

	// The second account puts its copy on a page, so its claim is a live one. The first account's
	// claim is not, and goes on the next sweep — but the file behind it must not, because the file
	// is shared and the other account is still using it.
	secondDevice.push(
		notebookChange("notebook-1", 0, "Work"),
		sectionChange("section-1", 0, "notebook-1", "Meetings"),
		pageChange("page-1", 0, "section-1", "Monday"),
		pageContentChangeWithBlobs("page-1", 0, []byte(`{"schema":3}`), digest),
	)

	first.sweepAttachments(0)

	if presence := firstDevice.blobPresence(digest); presence.status != http.StatusNotFound {
		t.Fatalf("the unreferenced claim survived the sweep: presence = %d", presence.status)
	}
	if stored := first.storedBlobFiles(); stored != 1 {
		t.Fatalf("the shared file was unlinked while another account still held it: %d files remain", stored)
	}
	downloaded := secondDevice.downloadBlob(digest)
	if downloaded.status != http.StatusOK || !bytes.Equal(downloaded.body, picture) {
		t.Fatalf("the second account can no longer read its picture: %d with %d bytes", downloaded.status, len(downloaded.body))
	}
}
