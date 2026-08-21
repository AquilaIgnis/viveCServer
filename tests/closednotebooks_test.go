package tests

import (
	"context"
	"errors"
	"testing"

	"github.com/AquilaIgnis/viveCServer/internal/store"
)

// The two timestamps a closed notebook carries, and the moment somebody deletes it. Spelled out so
// an assertion can say which of the three it expected to survive.
const (
	closedAtMillis    = 1_700_000_100_000
	cloudOnlyAtMillis = 1_700_000_200_000
	deletionAtMillis  = 1_700_000_500_000
)

// TestDeletingAClosedNotebookKeepsItHostedForEveryDevice is the whole feature in one test.
//
// A notebook the account has shelved is one whose contents may live only here, so a device asking
// to delete it is a device asking to erase something it does not have. The server keeps it, marks
// it cloud-hosted, and hands it back to every device — the one that pressed delete included — as a
// notebook that can be downloaded again.
func TestDeletingAClosedNotebookKeepsItHostedForEveryDevice(t *testing.T) {
	fixture := newSyncFixture(t)
	tablet := fixture.registerDevice("Studio tablet")
	phone := fixture.registerDevice("Phone")

	tablet.push(
		notebookChange("notebook-1", 0, "Field notes"),
		sectionChange("section-1", 0, "notebook-1", "August"),
		pageChange("page-1", 0, "section-1", "Rig notes"),
		pageContentChange("page-1", 0, []byte(`{"blocks":[]}`)),
	)
	tablet.push(closedNotebookChange("notebook-1", 1, "Field notes", closedAtMillis))

	// Everything the phone knows, so that what it pulls after the delete is only the delete.
	_, phoneCursor := phone.pullEverything(0, 100)

	result := tablet.push(deletedNotebookChange(
		closedNotebookChange("notebook-1", 2, "Field notes", closedAtMillis), deletionAtMillis))
	if len(result.Rejected) != 0 {
		t.Fatalf("the delete was rejected: %+v", result.Rejected)
	}
	if len(result.Applied) != 1 || result.Applied[0].Version != 3 {
		t.Fatalf("applied = %+v, want one notebook at version 3: a kept delete is still a write", result.Applied)
	}

	closedAt, cloudOnlyAt, deletedAt := fixture.notebookShelf("notebook-1")
	if deletedAt != nil {
		t.Fatalf("deleted_at = %d, want null: a closed notebook's delete must not become a tombstone", *deletedAt)
	}
	if closedAt == nil || *closedAt != closedAtMillis {
		t.Fatalf("closed_at = %v, want the moment it was shelved", closedAt)
	}
	if cloudOnlyAt == nil || *cloudOnlyAt != deletionAtMillis {
		t.Fatalf("cloud_only_at = %v, want the moment the device stopped holding it", cloudOnlyAt)
	}

	// The contents are what the retention is for. A tombstoned notebook would have taken these to
	// the archive, where the panel's permanent deletion can erase the lot.
	var storedBodies int
	if err := fixture.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM page_content WHERE account_id = $1::uuid AND page_id = 'page-1'`,
		fixture.accountID).Scan(&storedBodies); err != nil {
		t.Fatalf("counting stored bodies: %v", err)
	}
	if storedBodies != 1 {
		t.Fatalf("stored bodies = %d, want 1: the contents did not stay hosted", storedBodies)
	}

	// The other device learns the notebook moved to the cloud, not that it went away.
	delta, _ := phone.pullEverything(phoneCursor, 100)
	if len(delta) != 1 {
		t.Fatalf("the phone pulled %d changes, want only the notebook", len(delta))
	}
	pulled := delta[0]
	if pulled["deletedAt"] != nil {
		t.Fatalf("the phone pulled a tombstone: %+v", pulled)
	}
	if pulled["cloudOnlyAt"] != float64(deletionAtMillis) || pulled["closedAt"] != float64(closedAtMillis) {
		t.Fatalf("the phone pulled closedAt=%v cloudOnlyAt=%v, want the shelf the server stored",
			pulled["closedAt"], pulled["cloudOnlyAt"])
	}

	// And the device that pressed delete gets the same answer, which is how it puts the notebook
	// back on its own shelf instead of leaving a hole where it used to be.
	fromTablet, _ := tablet.pullEverything(2, 100)
	if len(fromTablet) == 0 || changesByID(fromTablet)["notebook-1"]["cloudOnlyAt"] != float64(deletionAtMillis) {
		t.Fatalf("the deleting device did not pull the notebook back: %+v", fromTablet)
	}
}

// TestDeletingAnOpenNotebookIsStillADelete guards the other half of the rule. Retention applies to
// the shelf and to nothing else: a notebook in the rail is one whose contents are on the devices,
// and its deletion is theirs to ask for.
func TestDeletingAnOpenNotebookIsStillADelete(t *testing.T) {
	fixture := newSyncFixture(t)
	tablet := fixture.registerDevice("Studio tablet")

	tablet.push(notebookChange("notebook-1", 0, "Work"))
	tablet.push(deletedNotebookChange(notebookChange("notebook-1", 1, "Work"), deletionAtMillis))

	closedAt, cloudOnlyAt, deletedAt := fixture.notebookShelf("notebook-1")
	if deletedAt == nil || *deletedAt != deletionAtMillis {
		t.Fatalf("deleted_at = %v, want the client's tombstone", deletedAt)
	}
	if closedAt != nil || cloudOnlyAt != nil {
		t.Fatalf("an open notebook was put on a shelf by deleting it: closed_at=%v cloud_only_at=%v", closedAt, cloudOnlyAt)
	}
}

// TestReopeningANotebookClearsItsShelf: the fields are nullable both ways, and a build that only
// ever set them would leave every notebook that was closed once closed for ever.
func TestReopeningANotebookClearsItsShelf(t *testing.T) {
	fixture := newSyncFixture(t)
	tablet := fixture.registerDevice("Studio tablet")

	tablet.push(notebookChange("notebook-1", 0, "Work"))
	tablet.push(closedNotebookChange("notebook-1", 1, "Work", closedAtMillis))

	reopened := notebookChange("notebook-1", 2, "Work")
	reopened["closedAt"] = nil
	reopened["cloudOnlyAt"] = nil
	tablet.push(reopened)

	closedAt, cloudOnlyAt, _ := fixture.notebookShelf("notebook-1")
	if closedAt != nil || cloudOnlyAt != nil {
		t.Fatalf("closed_at=%v cloud_only_at=%v after reopening, want both null", closedAt, cloudOnlyAt)
	}

	// And it is a delete again, because it is back in the rail.
	deletion := notebookChange("notebook-1", 3, "Work")
	deletion["closedAt"] = nil
	deletion["cloudOnlyAt"] = nil
	if result := tablet.push(deletedNotebookChange(deletion, deletionAtMillis)); len(result.Rejected) != 0 {
		t.Fatalf("deleting the reopened notebook was rejected: %+v", result.Rejected)
	}
	if _, _, deletedAt := fixture.notebookShelf("notebook-1"); deletedAt == nil {
		t.Fatal("a reopened notebook was retained instead of deleted")
	}
}

// TestTheShelfTravelsAsARecognisedFieldRatherThanAnUnknownOne: these two keys used to land in
// `extra`, which is where a field lives while the server has no opinion about it. Now that the
// server acts on them they must be in their own columns — and, critically, in *only* their own
// columns. A copy left in `extra` would be shadowed by the rendered change on every pull and would
// sit there disagreeing with the column that decides what a delete means.
func TestTheShelfTravelsAsARecognisedFieldRatherThanAnUnknownOne(t *testing.T) {
	fixture := newSyncFixture(t)
	tablet := fixture.registerDevice("Studio tablet")

	change := closedNotebookChange("notebook-1", 0, "Field notes", closedAtMillis)
	change["cloudOnlyAt"] = cloudOnlyAtMillis
	change["somethingNewer"] = "a field this build has never heard of"
	tablet.push(change)

	if extra := fixture.notebookExtra("notebook-1"); extra != `{"somethingNewer": "a field this build has never heard of"}` {
		t.Fatalf("extra = %s, want only the genuinely unrecognised field", extra)
	}

	delta, _ := tablet.pullEverything(0, 100)
	pulled := changesByID(delta)["notebook-1"]
	if pulled["closedAt"] != float64(closedAtMillis) || pulled["cloudOnlyAt"] != float64(cloudOnlyAtMillis) {
		t.Fatalf("pulled closedAt=%v cloudOnlyAt=%v, want what was pushed", pulled["closedAt"], pulled["cloudOnlyAt"])
	}
	if pulled["somethingNewer"] != "a field this build has never heard of" {
		t.Fatalf("the unrecognised field did not survive the round trip: %+v", pulled)
	}
}

// TestNotebookOverviewDividesSyncedFromCloudHosted: the admin panel's two groups come from one
// snapshot, so the counts cannot disagree with the lists they head.
func TestNotebookOverviewDividesSyncedFromCloudHosted(t *testing.T) {
	fixture := newSyncFixture(t)
	ctx := context.Background()
	tablet := fixture.registerDevice("Studio tablet")

	tablet.push(
		notebookChange("notebook-open", 0, "Work"),
		closedNotebookChange("notebook-closed", 0, "Old sketches", closedAtMillis),
		closedNotebookChange("notebook-cloud", 0, "Archive 2019", closedAtMillis),
		notebookChange("notebook-gone", 0, "Mistake"),
	)
	tablet.push(deletedNotebookChange(
		closedNotebookChange("notebook-cloud", 1, "Archive 2019", closedAtMillis), deletionAtMillis))
	tablet.push(deletedNotebookChange(notebookChange("notebook-gone", 1, "Mistake"), deletionAtMillis))

	overview, err := store.NotebookOverview(ctx, fixture.pool, fixture.accountID, 50)
	if err != nil {
		t.Fatalf("notebook overview: %v", err)
	}

	if overview.NotebookCount != 2 || len(overview.Notebooks) != 2 {
		t.Fatalf("synced count = %d with %d names, want 2 and 2", overview.NotebookCount, len(overview.Notebooks))
	}
	if overview.Notebooks[0].Name != "Old sketches" || !overview.Notebooks[0].Closed {
		t.Fatalf("first synced notebook = %+v, want the closed one first alphabetically and marked closed", overview.Notebooks[0])
	}
	if overview.Notebooks[1].Name != "Work" || overview.Notebooks[1].Closed {
		t.Fatalf("second synced notebook = %+v, want the open one unmarked", overview.Notebooks[1])
	}

	if overview.CloudNotebookCount != 1 || len(overview.CloudNotebooks) != 1 {
		t.Fatalf("cloud count = %d with %d names, want 1 and 1", overview.CloudNotebookCount, len(overview.CloudNotebooks))
	}
	if overview.CloudNotebooks[0].Name != "Archive 2019" || !overview.CloudNotebooks[0].Closed {
		t.Fatalf("cloud notebook = %+v, want Archive 2019 and closed", overview.CloudNotebooks[0])
	}

	if overview.ArchivedNotebookCount != 1 || len(overview.ArchivedNotebooks) != 1 {
		t.Fatalf("archived count = %d with %d names, want 1 and 1", overview.ArchivedNotebookCount, len(overview.ArchivedNotebooks))
	}
	if overview.ArchivedNotebooks[0].Name != "Mistake" {
		t.Fatalf("archived notebook = %+v, want the one that was deleted while open", overview.ArchivedNotebooks[0])
	}
}

// TestStopHostingACloudNotebookDeletesItThroughTheOrdinaryPath: the server owner is the only party
// left who can delete a cloud-hosted notebook, so the action has to reach the devices. It writes a
// real tombstone, which pulls like any other and then finishes in the archive.
func TestStopHostingACloudNotebookDeletesItThroughTheOrdinaryPath(t *testing.T) {
	fixture := newSyncFixture(t)
	ctx := context.Background()
	tablet := fixture.registerDevice("Studio tablet")
	phone := fixture.registerDevice("Phone")

	tablet.push(
		notebookChange("notebook-open", 0, "Work"),
		closedNotebookChange("notebook-cloud", 0, "Archive 2019", closedAtMillis),
		sectionChange("section-1", 0, "notebook-cloud", "August"),
		pageChange("page-1", 0, "section-1", "Rig notes"),
	)
	tablet.push(deletedNotebookChange(
		closedNotebookChange("notebook-cloud", 1, "Archive 2019", closedAtMillis), deletionAtMillis))

	_, tabletCursor := tablet.pullEverything(0, 100)
	_, phoneCursor := phone.pullEverything(0, 100)

	// A notebook the devices still hold is not this action's business: deleting it is theirs to ask
	// for, and the panel would be revoking a decision nobody made.
	if err := store.StopHostingCloudNotebook(ctx, fixture.pool, fixture.accountID, "notebook-open"); !errors.Is(err, store.ErrNotebookNotCloudHosted) {
		t.Fatalf("unhosting a synced notebook returned %v, want ErrNotebookNotCloudHosted", err)
	}
	if err := store.StopHostingCloudNotebook(ctx, fixture.pool, fixture.accountID, "no-such-notebook"); !errors.Is(err, store.ErrNotebookNotFound) {
		t.Fatalf("unhosting an unknown notebook returned %v, want ErrNotebookNotFound", err)
	}

	if err := store.StopHostingCloudNotebook(ctx, fixture.pool, fixture.accountID, "notebook-cloud"); err != nil {
		t.Fatalf("unhosting a cloud notebook: %v", err)
	}
	// Twice is not idempotent here, and should not be: the second press is acting on markup that
	// describes a row which has already moved to the archive.
	if err := store.StopHostingCloudNotebook(ctx, fixture.pool, fixture.accountID, "notebook-cloud"); !errors.Is(err, store.ErrNotebookNotCloudHosted) {
		t.Fatalf("unhosting an archived notebook returned %v, want ErrNotebookNotCloudHosted", err)
	}

	// It is an ordinary change, so both devices see it by pulling and nothing else.
	for _, pull := range []struct {
		device *deviceClient
		cursor int64
	}{{tablet, tabletCursor}, {phone, phoneCursor}} {
		delta, cursor := pull.device.pullEverything(pull.cursor, 100)
		if len(delta) != 1 {
			t.Fatalf("%s pulled %d changes, want only the tombstone", pull.device.name, len(delta))
		}
		if delta[0]["id"] != "notebook-cloud" || delta[0]["deletedAt"] != float64(*mustDeletedAt(t, fixture)) {
			t.Fatalf("%s pulled %+v, want the notebook's tombstone", pull.device.name, delta[0])
		}
		// One more, presenting the cursor the tombstone arrived at. Receiving a response is not an
		// acknowledgement -- it can be lost on the way -- so the server only counts a device as
		// having the deletion once the device asks for what comes after it (migration 00006).
		pull.device.pull(cursor, 100)
	}

	overview, err := store.NotebookOverview(ctx, fixture.pool, fixture.accountID, 50)
	if err != nil {
		t.Fatalf("notebook overview: %v", err)
	}
	if overview.CloudNotebookCount != 0 || overview.ArchivedNotebookCount != 1 {
		t.Fatalf("cloud=%d archived=%d after unhosting, want 0 and 1",
			overview.CloudNotebookCount, overview.ArchivedNotebookCount)
	}

	// And the archive's own action finishes the job, subtree and all.
	if err := store.DeleteArchivedNotebook(ctx, fixture.pool, fixture.accountID, "notebook-cloud"); err != nil {
		t.Fatalf("permanently deleting the unhosted notebook: %v", err)
	}
	var remainingPages int
	if err := fixture.pool.QueryRow(ctx,
		`SELECT count(*) FROM pages WHERE account_id = $1::uuid AND id = 'page-1'`,
		fixture.accountID).Scan(&remainingPages); err != nil {
		t.Fatalf("counting pages after permanent deletion: %v", err)
	}
	if remainingPages != 0 {
		t.Fatalf("pages left behind = %d, want 0", remainingPages)
	}
}

// mustDeletedAt reads the tombstone the admin action stamped, which uses the server's own clock
// rather than a client's and so cannot be written into the test as a literal.
func mustDeletedAt(t *testing.T, fixture *syncFixture) *int64 {
	t.Helper()
	_, _, deletedAt := fixture.notebookShelf("notebook-cloud")
	if deletedAt == nil {
		t.Fatal("the notebook carries no tombstone")
	}
	return deletedAt
}
