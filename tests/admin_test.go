package tests

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/AquilaIgnis/viveCServer/internal/store"
	"github.com/AquilaIgnis/viveCServer/internal/syncengine"
)

// TestRemovingRevokedDevicesLeavesTheNotesAlone is why this test needs a real database rather than
// a fake: what the admin panel calls "Remove" is a SQL DELETE against a row that six other tables
// point at. Whether that deletes a note is a property of the foreign keys in migrations/, so only
// PostgreSQL can answer it.
func TestRemovingRevokedDevicesLeavesTheNotesAlone(t *testing.T) {
	fixture := newSyncFixture(t)
	ctx := context.Background()

	retired := fixture.registerDevice("Retired phone")
	kept := fixture.registerDevice("Studio tablet")

	// The device that is about to disappear is the last writer of a notebook, and has a replayable
	// batch of its own. Both of those are rows that reference it.
	batchID := newBatchID(t)
	retired.pushBatch(batchID, notebookChange("notebook-1", 0, "Field notes"))

	// A live device is not removable: that would be a revocation nobody asked for.
	if err := store.DeleteRevokedDevice(ctx, fixture.pool, fixture.accountID, retired.deviceID); !errors.Is(err, store.ErrDeviceStillActive) {
		t.Fatalf("removing a live device: %v, want ErrDeviceStillActive", err)
	}

	if err := store.RevokeDevice(ctx, fixture.pool, fixture.accountID, retired.deviceID); err != nil {
		t.Fatalf("revoking the device: %v", err)
	}
	if err := store.DeleteRevokedDevice(ctx, fixture.pool, fixture.accountID, retired.deviceID); err != nil {
		t.Fatalf("removing the revoked device: %v", err)
	}

	// The note survived; only the attribution went, which is what ON DELETE SET NULL promises.
	var lastWriter *string
	var name string
	if err := fixture.pool.QueryRow(ctx,
		`SELECT name, last_writer::text FROM notebooks WHERE account_id = $1::uuid AND id = 'notebook-1'`,
		fixture.accountID).Scan(&name, &lastWriter); err != nil {
		t.Fatalf("reading the notebook back: %v", err)
	}
	if name != "Field notes" {
		t.Fatalf("notebook name = %q, want it untouched by a device removal", name)
	}
	if lastWriter != nil {
		t.Fatalf("last_writer = %q, want NULL after its device was removed", *lastWriter)
	}

	// The removed device's idempotency rows went with it. That costs nothing: a revoked device has
	// no retry left to replay.
	var replayableBatches int64
	if err := fixture.pool.QueryRow(ctx,
		`SELECT count(*) FROM applied_batches WHERE batch_id = $1::uuid`, batchID).Scan(&replayableBatches); err != nil {
		t.Fatalf("counting applied batches: %v", err)
	}
	if replayableBatches != 0 {
		t.Fatalf("applied_batches rows left for a removed device = %d, want 0", replayableBatches)
	}

	// Removing it again is "no such device" rather than an error the panel has to explain twice.
	if err := store.DeleteRevokedDevice(ctx, fixture.pool, fixture.accountID, retired.deviceID); !errors.Is(err, store.ErrDeviceNotFound) {
		t.Fatalf("removing it twice: %v, want ErrDeviceNotFound", err)
	}

	// And the device that was never revoked still syncs.
	if _, status, err := kept.tryPull(0, 0); err != nil || status != http.StatusOK {
		t.Fatalf("the surviving device pull = %d (%v), want 200", status, err)
	}
}

// TestRemovingRevokedDevicesIsScopedToOneAccount: every device operation is account-scoped, and a
// delete is the one where getting that wrong is unrecoverable.
func TestRemovingRevokedDevicesIsScopedToOneAccount(t *testing.T) {
	ctx := context.Background()
	owner := newSyncFixture(t)
	stranger := newSyncFixture(t)

	ownerDevice := owner.registerDevice("Owner phone")
	if err := store.RevokeDevice(ctx, owner.pool, owner.accountID, ownerDevice.deviceID); err != nil {
		t.Fatalf("revoking: %v", err)
	}

	if err := store.DeleteRevokedDevice(ctx, owner.pool, stranger.accountID, ownerDevice.deviceID); !errors.Is(err, store.ErrDeviceNotFound) {
		t.Fatalf("cross-account removal: %v, want ErrDeviceNotFound", err)
	}
	if removedCount, err := store.DeleteRevokedDevices(ctx, owner.pool, stranger.accountID); err != nil || removedCount != 0 {
		t.Fatalf("cross-account bulk removal removed %d rows (%v), want 0", removedCount, err)
	}

	devices, err := store.ListDevices(ctx, owner.pool, owner.accountID)
	if err != nil || len(devices) != 1 {
		t.Fatalf("owner devices = %d (%v), want the row still there", len(devices), err)
	}
}

// TestBulkRemovalTakesOnlyRevokedDevices covers the panel-heading button, which is the one that
// could quietly take a device the operator still uses.
func TestBulkRemovalTakesOnlyRevokedDevices(t *testing.T) {
	fixture := newSyncFixture(t)
	ctx := context.Background()

	first := fixture.registerDevice("Old phone")
	second := fixture.registerDevice("Lost tablet")
	live := fixture.registerDevice("Studio tablet")
	for _, device := range []*deviceClient{first, second} {
		if err := store.RevokeDevice(ctx, fixture.pool, fixture.accountID, device.deviceID); err != nil {
			t.Fatalf("revoking %s: %v", device.name, err)
		}
	}

	removedCount, err := store.DeleteRevokedDevices(ctx, fixture.pool, fixture.accountID)
	if err != nil || removedCount != 2 {
		t.Fatalf("bulk removal removed %d rows (%v), want 2", removedCount, err)
	}

	devices, err := store.ListDevices(ctx, fixture.pool, fixture.accountID)
	if err != nil {
		t.Fatalf("listing devices: %v", err)
	}
	if len(devices) != 1 || devices[0].ID != live.deviceID {
		t.Fatalf("devices left = %+v, want only the live one", devices)
	}

	// A second clear is a success that removes nothing: the asked-for state already holds.
	if removedCount, err := store.DeleteRevokedDevices(ctx, fixture.pool, fixture.accountID); err != nil || removedCount != 0 {
		t.Fatalf("repeat bulk removal removed %d rows (%v), want 0", removedCount, err)
	}
}

// TestNotebookOverviewCountsWhatTheClientsShow: the dashboard number has to agree with what the app
// displays, and the app does not show tombstones. It also must not count another account's rows.
func TestNotebookOverviewCountsWhatTheClientsShow(t *testing.T) {
	fixture := newSyncFixture(t)
	stranger := newSyncFixture(t)
	ctx := context.Background()

	device := fixture.registerDevice("Studio tablet")
	device.push(
		notebookChange("notebook-1", 0, "Work"),
		notebookChange("notebook-2", 0, "Admin"),
		notebookChange("notebook-3", 0, "Field notes"),
	)
	stranger.registerDevice("Somebody else").push(notebookChange("notebook-1", 0, "Not yours"))

	overview, err := store.NotebookOverview(ctx, fixture.pool, fixture.accountID, 50)
	if err != nil {
		t.Fatalf("notebook overview: %v", err)
	}
	if overview.NotebookCount != 3 || len(overview.Notebooks) != 3 {
		t.Fatalf("count = %d with %d names, want 3 and 3", overview.NotebookCount, len(overview.Notebooks))
	}
	// Ordered by name, so the list reads the way somebody looking for one would scan it.
	if overview.Notebooks[0].Name != "Admin" || overview.Notebooks[1].Name != "Field notes" || overview.Notebooks[2].Name != "Work" {
		t.Fatalf("names = %q, %q, %q, want them alphabetical", overview.Notebooks[0].Name, overview.Notebooks[1].Name, overview.Notebooks[2].Name)
	}
	if overview.Notebooks[0].ServerUpdatedAt.IsZero() {
		t.Fatal("a summary carries no server timestamp, so the list cannot say when it changed")
	}
	if overview.ArchivedNotebookCount != 0 || len(overview.ArchivedNotebooks) != 0 {
		t.Fatal("live notebooks appeared in the archive")
	}

	// Deleting one is a tombstone write, not a DELETE. It stays pullable and stops being counted.
	deletion := notebookChange("notebook-2", 1, "Admin")
	deletion["deletedAt"] = 1_700_000_001_000
	device.push(deletion)

	overview, err = store.NotebookOverview(ctx, fixture.pool, fixture.accountID, 50)
	if err != nil {
		t.Fatalf("notebook overview after a delete: %v", err)
	}
	if overview.NotebookCount != 2 || len(overview.Notebooks) != 2 {
		t.Fatalf("count after a delete = %d with %d names, want 2 and 2", overview.NotebookCount, len(overview.Notebooks))
	}
	for _, summary := range overview.Notebooks {
		if summary.Name == "Admin" {
			t.Fatal("a deleted notebook is still being listed")
		}
	}
	if overview.ArchivedNotebookCount != 1 || len(overview.ArchivedNotebooks) != 1 {
		t.Fatalf("archive count = %d with %d names, want 1 and 1", overview.ArchivedNotebookCount, len(overview.ArchivedNotebooks))
	}
	if overview.ArchivedNotebooks[0].Name != "Admin" || overview.ArchivedNotebooks[0].ServerUpdatedAt.IsZero() {
		t.Fatalf("archived notebook = %+v, want Admin with the server archival time", overview.ArchivedNotebooks[0])
	}

	// It remains the sync tombstone rather than being copied to a dashboard-only table. A device
	// that was offline at deletion time can therefore still learn that the notebook is gone.
	var deletedAt *int64
	if err := fixture.pool.QueryRow(ctx,
		`SELECT deleted_at FROM notebooks WHERE account_id = $1::uuid AND id = 'notebook-2'`,
		fixture.accountID).Scan(&deletedAt); err != nil {
		t.Fatalf("reading archived notebook tombstone: %v", err)
	}
	if deletedAt == nil || *deletedAt != 1_700_000_001_000 {
		t.Fatalf("archived notebook deleted_at = %v, want the client tombstone", deletedAt)
	}
}

// TestNotebookOverviewCountsBeyondItsLimit: the count is the whole truth even when the list is not,
// which is what lets the page say how many names it left out.
func TestNotebookOverviewCountsBeyondItsLimit(t *testing.T) {
	fixture := newSyncFixture(t)
	device := fixture.registerDevice("Studio tablet")

	changes := make([]map[string]any, 0, 5)
	for index := range 5 {
		changes = append(changes, notebookChange(fmt.Sprintf("notebook-%d", index), 0, fmt.Sprintf("Notebook %d", index)))
	}
	device.push(changes...)

	overview, err := store.NotebookOverview(context.Background(), fixture.pool, fixture.accountID, 2)
	if err != nil {
		t.Fatalf("notebook overview: %v", err)
	}
	if overview.NotebookCount != 5 {
		t.Fatalf("count = %d, want 5: the total must not stop at the limit", overview.NotebookCount)
	}
	if len(overview.Notebooks) != 2 {
		t.Fatalf("names = %d, want the limit of 2", len(overview.Notebooks))
	}
}

// TestPermanentlyDeletingAnArchivedNotebookRemovesItsWholeSubtree proves the button's destructive
// promise against real foreign keys. It also pins the sync interlock: the tombstone cannot disappear
// until every active device has acknowledged it on a later pull.
func TestPermanentlyDeletingAnArchivedNotebookRemovesItsWholeSubtree(t *testing.T) {
	fixture := newSyncFixture(t)
	stranger := newSyncFixture(t)
	ctx := context.Background()

	tablet := fixture.registerDevice("Studio tablet")
	phone := fixture.registerDevice("Phone")
	created := tablet.push(
		notebookChange("notebook-delete", 0, "Old field notes"),
		notebookChange("notebook-keep", 0, "Keep me"),
		sectionChange("section-delete", 0, "notebook-delete", "Ideas"),
		pageChange("page-delete", 0, "section-delete", "Canvas"),
		pageContentChange("page-delete", 0, []byte(`{"type":"doc"}`)),
		inkStrokeChange("stroke-delete", 0, "page-delete", 1, []byte{1, 2, 3}),
		inkEraseChange("erase-delete", 0, "page-delete", []string{"stroke-delete"}),
		inkMoveChange("move-delete", 0, "page-delete", []string{"stroke-delete"}),
	)
	if len(created.Applied) != 8 || len(created.Rejected) != 0 {
		t.Fatalf("creating notebook subtree applied %d rejected %d, want 8 and 0: %+v", len(created.Applied), len(created.Rejected), created.Rejected)
	}
	// Both active devices know the initial tree at sequence 1.
	tablet.pull(0, 0)
	phone.pull(0, 0)

	// A live notebook can never be reached through the archive action, even with a forged form.
	if err := store.DeleteArchivedNotebook(ctx, fixture.pool, fixture.accountID, "notebook-keep"); !errors.Is(err, store.ErrNotebookNotArchived) {
		t.Fatalf("deleting a live notebook: %v, want ErrNotebookNotArchived", err)
	}
	// Account scoping deliberately reports another account's unknown id as not found.
	if err := store.DeleteArchivedNotebook(ctx, fixture.pool, stranger.accountID, "notebook-delete"); !errors.Is(err, store.ErrNotebookNotFound) {
		t.Fatalf("cross-account notebook deletion: %v, want ErrNotebookNotFound", err)
	}

	deletion := notebookChange("notebook-delete", 1, "Old field notes")
	deletion["deletedAt"] = 1_700_000_001_000
	deleted := tablet.push(deletion)
	if len(deleted.Applied) != 1 || deleted.Cursor != 2 {
		t.Fatalf("archiving notebook = %+v, want one write at cursor 2", deleted)
	}

	// Neither device has pulled the tombstone, and neither is asked to. The whole point of the
	// purge log is that this is the operator's call and no device gets a vote in it.
	if err := store.DeleteArchivedNotebook(ctx, fixture.pool, fixture.accountID, "notebook-delete"); err != nil {
		t.Fatalf("permanently deleting an archive no device has pulled: %v", err)
	}

	for _, entity := range []struct {
		table string
		id    string
	}{
		{table: "notebooks", id: "notebook-delete"},
		{table: "sections", id: "section-delete"},
		{table: "pages", id: "page-delete"},
		{table: "page_content", id: "page-delete"},
		{table: "ink_strokes", id: "stroke-delete"},
		{table: "ink_erases", id: "erase-delete"},
		{table: "ink_moves", id: "move-delete"},
	} {
		var rowsLeft int64
		statement := fmt.Sprintf(`SELECT count(*) FROM %s WHERE account_id = $1::uuid AND id = $2`, entity.table)
		if err := fixture.pool.QueryRow(ctx, statement, fixture.accountID, entity.id).Scan(&rowsLeft); err != nil {
			t.Fatalf("counting %s after cascade: %v", entity.table, err)
		}
		if rowsLeft != 0 {
			t.Errorf("%s still has %d rows for %q after permanent notebook deletion", entity.table, rowsLeft, entity.id)
		}
	}

	var keptNotebooks int64
	if err := fixture.pool.QueryRow(ctx,
		`SELECT count(*) FROM notebooks WHERE account_id = $1::uuid AND id = 'notebook-keep'`,
		fixture.accountID,
	).Scan(&keptNotebooks); err != nil {
		t.Fatalf("checking unrelated notebook: %v", err)
	}
	if keptNotebooks != 1 {
		t.Fatal("permanent deletion removed an unrelated notebook")
	}

	// The archive tab is empty now, because the row that was projected into it is gone rather than
	// merely marked. Nothing is waiting on anything.
	overview, err := store.NotebookOverview(ctx, fixture.pool, fixture.accountID, 50)
	if err != nil {
		t.Fatalf("loading the overview after permanent deletion: %v", err)
	}
	if len(overview.ArchivedNotebooks) != 0 || overview.ArchivedNotebookCount != 0 {
		t.Fatalf("archive after permanent deletion = %+v, want nothing", overview.ArchivedNotebooks)
	}

	// Both devices are still at cursor 1: they never saw the tombstone at 2 and there is no
	// tombstone left to see. What reaches them instead is the purge, under the cursor the deletion
	// allocated for it.
	for _, device := range []*deviceClient{tablet, phone} {
		delta := device.pull(1, 0)
		if len(delta.Purges) != 1 {
			t.Fatalf("%s pulled %d purges, want 1: %s", device.name, len(delta.Purges), delta.Purges)
		}
		purge := decodeChangeObject(t, delta.Purges[0])
		if purge["kind"] != "notebook" || purge["id"] != "notebook-delete" {
			t.Fatalf("%s pulled purge %+v, want the deleted notebook", device.name, purge)
		}
		if purge["seq"] != float64(delta.Cursor) {
			t.Fatalf("%s pulled purge at seq %v under cursor %d", device.name, purge["seq"], delta.Cursor)
		}
		if delta.HasMore {
			t.Fatalf("%s has more after the purge", device.name)
		}
		// The tombstone is not among the changes: there is no row left to render one from. The
		// purge is the entire notification, which is why it has to be its own array rather than
		// something a client could mistake for an ordinary delete.
		for _, raw := range delta.Changes {
			if decodeChangeObject(t, raw)["id"] == "notebook-delete" {
				t.Fatalf("%s pulled a change for a permanently deleted notebook: %s", device.name, raw)
			}
		}
	}
}

// TestAPurgedNotebookCannotBePushedBackByADeviceThatMissedIt: the gravestone half of the log. A
// device that was offline when the operator erased a notebook still holds it, and the client's
// answer to "this account has no such entity" is to push it again as new — which would hand the
// account back exactly what it deleted, from the one device that had not heard.
func TestAPurgedNotebookCannotBePushedBackByADeviceThatMissedIt(t *testing.T) {
	fixture := newSyncFixture(t)
	ctx := context.Background()

	tablet := fixture.registerDevice("Studio tablet")
	tablet.push(
		notebookChange("notebook-gone", 0, "Field notes"),
		sectionChange("section-gone", 0, "notebook-gone", "Ideas"),
	)
	tablet.push(deletedNotebookChange(notebookChange("notebook-gone", 1, "Field notes"), 1_700_000_001_000))

	if err := store.DeleteArchivedNotebook(ctx, fixture.pool, fixture.accountID, "notebook-gone"); err != nil {
		t.Fatalf("permanently deleting the archive: %v", err)
	}

	// The stale edit the device had queued. Its baseVersion is the version it last saw.
	stale := tablet.push(notebookChange("notebook-gone", 2, "Field notes"))
	if len(stale.Applied) != 0 || len(stale.Rejected) != 1 {
		t.Fatalf("pushing a purged notebook = %+v, want one rejection", stale)
	}
	if stale.Rejected[0].Reason != syncengine.ReasonPurged {
		t.Fatalf("pushing a purged notebook was refused %q, want %q", stale.Rejected[0].Reason, syncengine.ReasonPurged)
	}

	// And again as new, which is what a client does when it is told the server has no such entity.
	// This is the push the log actually exists to stop.
	asNew := tablet.push(notebookChange("notebook-gone", 0, "Field notes"))
	if len(asNew.Applied) != 0 || len(asNew.Rejected) != 1 || asNew.Rejected[0].Reason != syncengine.ReasonPurged {
		t.Fatalf("re-creating a purged notebook = %+v, want one purged rejection", asNew)
	}

	// A tombstone for it is refused for the same reason: there is nothing left to tombstone, and
	// storing one would put a row back into the archive the operator has already emptied.
	asTombstone := tablet.push(deletedNotebookChange(notebookChange("notebook-gone", 0, "Field notes"), 1_700_000_002_000))
	if len(asTombstone.Rejected) != 1 || asTombstone.Rejected[0].Reason != syncengine.ReasonPurged {
		t.Fatalf("tombstoning a purged notebook = %+v, want one purged rejection", asTombstone)
	}

	var notebookRows int64
	if err := fixture.pool.QueryRow(ctx,
		`SELECT count(*) FROM notebooks WHERE account_id = $1::uuid AND id = 'notebook-gone'`,
		fixture.accountID,
	).Scan(&notebookRows); err != nil {
		t.Fatalf("counting the purged notebook: %v", err)
	}
	if notebookRows != 0 {
		t.Fatal("a purged notebook came back")
	}

	// A different notebook in the same batch is unaffected: the refusal is about one retired id,
	// not about the push.
	mixed := tablet.push(
		notebookChange("notebook-gone", 0, "Field notes"),
		notebookChange("notebook-fresh", 0, "New field notes"),
	)
	if len(mixed.Applied) != 1 || mixed.Applied[0].ID != "notebook-fresh" {
		t.Fatalf("a batch beside a purged id = %+v, want the fresh notebook applied", mixed)
	}
	if len(mixed.Rejected) != 1 || mixed.Rejected[0].ID != "notebook-gone" {
		t.Fatalf("a batch beside a purged id = %+v, want only the purged id refused", mixed)
	}
}

// TestPurgesAreScopedToOneAccountAndDrainOnce: the log is account-keyed like everything else, and a
// device that has already been told does not keep being told.
func TestPurgesAreScopedToOneAccountAndDrainOnce(t *testing.T) {
	fixture := newSyncFixture(t)
	stranger := newSyncFixtureOnStore(t, fixture)
	ctx := context.Background()

	tablet := fixture.registerDevice("Studio tablet")
	tablet.push(notebookChange("notebook-gone", 0, "Field notes"))
	tablet.push(deletedNotebookChange(notebookChange("notebook-gone", 1, "Field notes"), 1_700_000_001_000))

	strangerTablet := stranger.registerDevice("Someone else's tablet")
	strangerTablet.push(notebookChange("notebook-gone", 0, "Same id, other account"))

	if err := store.DeleteArchivedNotebook(ctx, fixture.pool, fixture.accountID, "notebook-gone"); err != nil {
		t.Fatalf("permanently deleting the archive: %v", err)
	}

	// The other account shares the id and hears nothing, and its own notebook is untouched.
	strangerDelta := strangerTablet.pull(0, 0)
	if len(strangerDelta.Purges) != 0 {
		t.Fatalf("another account pulled %d purges, want 0: %s", len(strangerDelta.Purges), strangerDelta.Purges)
	}
	if len(strangerDelta.Changes) != 1 {
		t.Fatalf("another account's notebook = %+v, want it intact", strangerDelta.Changes)
	}

	purgeSeq := tablet.pull(0, 0)
	if len(purgeSeq.Purges) != 1 {
		t.Fatalf("first pull carried %d purges, want 1", len(purgeSeq.Purges))
	}
	// Read at the cursor it was delivered under: the window is half-open, so a client that stored
	// the cursor does not receive it a second time.
	drained := tablet.pull(purgeSeq.Cursor, 0)
	if len(drained.Purges) != 0 {
		t.Fatalf("a second pull repeated %d purges: %s", len(drained.Purges), drained.Purges)
	}
}

// TestAPurgeIsPrunedOnceEveryActiveDeviceHasAcknowledgedIt: the interlock that used to block the
// operator now decides only how long a hundred bytes of log are worth keeping.
func TestAPurgeIsPrunedOnceEveryActiveDeviceHasAcknowledgedIt(t *testing.T) {
	fixture := newSyncFixture(t)
	ctx := context.Background()

	tablet := fixture.registerDevice("Studio tablet")
	phone := fixture.registerDevice("Phone")
	tablet.push(notebookChange("notebook-first", 0, "Field notes"))
	tablet.push(deletedNotebookChange(notebookChange("notebook-first", 1, "Field notes"), 1_700_000_001_000))
	if err := store.DeleteArchivedNotebook(ctx, fixture.pool, fixture.accountID, "notebook-first"); err != nil {
		t.Fatalf("permanently deleting the first archive: %v", err)
	}

	// The tablet drains the purge and acknowledges it by presenting the cursor on a later pull. The
	// phone has not been switched on since, so the log has to stay.
	firstPurgeCursor := tablet.pull(0, 0).Cursor
	tablet.pull(firstPurgeCursor, 0)

	tablet.push(notebookChange("notebook-second", 0, "Other notes"))
	tablet.push(deletedNotebookChange(notebookChange("notebook-second", 1, "Other notes"), 1_700_000_002_000))
	if err := store.DeleteArchivedNotebook(ctx, fixture.pool, fixture.accountID, "notebook-second"); err != nil {
		t.Fatalf("permanently deleting the second archive: %v", err)
	}
	if held := fixture.purgedIDs(); len(held) != 2 {
		t.Fatalf("purge log holds %v, want both notebooks while a device is behind", held)
	}

	// The phone catches up and acknowledges. The next permanent deletion is what sweeps the log,
	// which is why the count is read after one rather than by a background job nobody scheduled.
	phoneCursor := phone.pull(0, 0).Cursor
	phone.pull(phoneCursor, 0)
	tablet.pull(phoneCursor, 0)

	tablet.push(notebookChange("notebook-third", 0, "Third notes"))
	tablet.push(deletedNotebookChange(notebookChange("notebook-third", 1, "Third notes"), 1_700_000_003_000))
	if err := store.DeleteArchivedNotebook(ctx, fixture.pool, fixture.accountID, "notebook-third"); err != nil {
		t.Fatalf("permanently deleting the third archive: %v", err)
	}
	held := fixture.purgedIDs()
	if len(held) != 1 || held[0] != "notebook-third" {
		t.Fatalf("purge log holds %v, want only the deletion nobody has acknowledged yet", held)
	}
}

// TestARevokedDeviceDoesNotHoldThePurgeLogOpen: revocation is permanent, so a revoked device can
// neither pull the purge nor push the id back, and nothing is waiting for it.
func TestARevokedDeviceDoesNotHoldThePurgeLogOpen(t *testing.T) {
	fixture := newSyncFixture(t)
	ctx := context.Background()

	tablet := fixture.registerDevice("Studio tablet")
	lost := fixture.registerDevice("Lost tablet")
	tablet.push(notebookChange("notebook-first", 0, "Field notes"))
	tablet.push(deletedNotebookChange(notebookChange("notebook-first", 1, "Field notes"), 1_700_000_001_000))
	if err := store.DeleteArchivedNotebook(ctx, fixture.pool, fixture.accountID, "notebook-first"); err != nil {
		t.Fatalf("permanently deleting the first archive: %v", err)
	}

	if _, err := fixture.pool.Exec(ctx,
		`UPDATE devices SET revoked_at = now() WHERE id = $1::uuid`, lost.deviceID,
	); err != nil {
		t.Fatalf("revoking the lost device: %v", err)
	}

	firstPurgeCursor := tablet.pull(0, 0).Cursor
	tablet.pull(firstPurgeCursor, 0)

	tablet.push(notebookChange("notebook-second", 0, "Other notes"))
	tablet.push(deletedNotebookChange(notebookChange("notebook-second", 1, "Other notes"), 1_700_000_002_000))
	if err := store.DeleteArchivedNotebook(ctx, fixture.pool, fixture.accountID, "notebook-second"); err != nil {
		t.Fatalf("permanently deleting the second archive: %v", err)
	}

	held := fixture.purgedIDs()
	if len(held) != 1 || held[0] != "notebook-second" {
		t.Fatalf("purge log holds %v; a revoked device held it open", held)
	}
}
