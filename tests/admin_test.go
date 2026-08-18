package tests

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/AquilaIgnis/viveCServer/internal/store"
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

	// The client that pushed the delete knows its request succeeded, but last_pulled_seq records a
	// cursor only when the device presents it on a later pull. Both devices must actually receive,
	// commit, and then acknowledge sequence 2 before the tombstone can be discarded.
	if err := store.DeleteArchivedNotebook(ctx, fixture.pool, fixture.accountID, "notebook-delete"); !errors.Is(err, store.ErrNotebookDeletionNotSynced) {
		t.Fatalf("deleting before devices pulled the tombstone: %v, want ErrNotebookDeletionNotSynced", err)
	}
	overview, err := store.NotebookOverview(ctx, fixture.pool, fixture.accountID, 50)
	if err != nil {
		t.Fatalf("loading archive readiness: %v", err)
	}
	if len(overview.ArchivedNotebooks) != 1 || overview.ArchivedNotebooks[0].ReadyForPermanentDeletion {
		t.Fatalf("archive before device pulls = %+v, want one disabled row", overview.ArchivedNotebooks)
	}

	tablet.pull(1, 0)
	phone.pull(1, 0)
	// Receiving the response is not yet an acknowledgement from the server's point of view. A
	// dropped connection can fail after the server prepares a response, so each client proves local
	// commit by using cursor 2 as `since` on its next ordinary poll.
	if err := store.DeleteArchivedNotebook(ctx, fixture.pool, fixture.accountID, "notebook-delete"); !errors.Is(err, store.ErrNotebookDeletionNotSynced) {
		t.Fatalf("deleting before devices acknowledged the tombstone: %v, want ErrNotebookDeletionNotSynced", err)
	}
	tablet.pull(2, 0)
	phone.pull(2, 0)
	overview, err = store.NotebookOverview(ctx, fixture.pool, fixture.accountID, 50)
	if err != nil {
		t.Fatalf("loading acknowledged archive: %v", err)
	}
	if len(overview.ArchivedNotebooks) != 1 || !overview.ArchivedNotebooks[0].ReadyForPermanentDeletion {
		t.Fatalf("archive after device pulls = %+v, want one deletable row", overview.ArchivedNotebooks)
	}

	if err := store.DeleteArchivedNotebook(ctx, fixture.pool, fixture.accountID, "notebook-delete"); err != nil {
		t.Fatalf("permanently deleting acknowledged archive: %v", err)
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
}
