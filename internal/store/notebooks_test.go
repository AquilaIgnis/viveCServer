package store

import (
	"encoding/json"
	"testing"
)

// storedNotebook renders the parts of a stored row that RetainOnDelete reads, the way a pull would.
func storedNotebook(closedAt *int64, cloudOnlyAt *int64, deletedAt *int64) json.RawMessage {
	encoded, err := json.Marshal(map[string]any{
		"kind":        "notebook",
		"id":          "notebook-1",
		"version":     3,
		"name":        "Field notes",
		"closedAt":    closedAt,
		"cloudOnlyAt": cloudOnlyAt,
		"deletedAt":   deletedAt,
	})
	if err != nil {
		panic(err)
	}
	return encoded
}

func timestamp(value int64) *int64 { return &value }

// TestRetainOnDeleteKeepsAClosedNotebook pins the rule the whole feature rests on: a closed
// notebook's delete means the device is finished with it, not that the account is.
func TestRetainOnDeleteKeepsAClosedNotebook(t *testing.T) {
	const deletedAt = 1_700_000_500_000

	cases := []struct {
		name            string
		pushed          NotebookFields
		stored          json.RawMessage
		wantRetained    bool
		wantClosedAt    *int64
		wantCloudOnlyAt *int64
	}{
		{
			name:         "an open notebook is deleted",
			pushed:       NotebookFields{Name: "Field notes"},
			stored:       storedNotebook(nil, nil, nil),
			wantRetained: false,
		},
		{
			name:            "closed by this very change",
			pushed:          NotebookFields{Name: "Field notes", ClosedAt: timestamp(1_700_000_400_000)},
			stored:          storedNotebook(nil, nil, nil),
			wantRetained:    true,
			wantClosedAt:    timestamp(1_700_000_400_000),
			wantCloudOnlyAt: timestamp(deletedAt),
		},
		{
			// The ordinary case: a notebook closed in some earlier batch, whose delete carries
			// nothing about the shelf because the client has not touched it since.
			name:            "closed by an earlier change",
			pushed:          NotebookFields{Name: "Field notes"},
			stored:          storedNotebook(timestamp(1_700_000_100_000), nil, nil),
			wantRetained:    true,
			wantClosedAt:    timestamp(1_700_000_100_000),
			wantCloudOnlyAt: timestamp(deletedAt),
		},
		{
			// Already moved to the cloud, so the moment the devices stopped holding it is a fact
			// that already happened. Restamping it would date the notebook by the delete instead.
			name:            "already cloud-hosted",
			pushed:          NotebookFields{Name: "Field notes"},
			stored:          storedNotebook(timestamp(1_700_000_100_000), timestamp(1_700_000_200_000), nil),
			wantRetained:    true,
			wantClosedAt:    timestamp(1_700_000_100_000),
			wantCloudOnlyAt: timestamp(1_700_000_200_000),
		},
		{
			name:         "the account has no such notebook",
			pushed:       NotebookFields{Name: "Field notes", ClosedAt: timestamp(1_700_000_400_000)},
			stored:       nil,
			wantRetained: false,
		},
		{
			name:         "the stored row is already a tombstone",
			pushed:       NotebookFields{Name: "Field notes", ClosedAt: timestamp(1_700_000_400_000)},
			stored:       storedNotebook(timestamp(1_700_000_100_000), nil, timestamp(1_700_000_300_000)),
			wantRetained: false,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fields := testCase.pushed
			retained := fields.RetainOnDelete(testCase.stored, deletedAt)

			if retained != testCase.wantRetained {
				t.Fatalf("retained = %t, want %t", retained, testCase.wantRetained)
			}
			if !retained {
				return
			}
			if !sameTimestamp(fields.ClosedAt, testCase.wantClosedAt) {
				t.Errorf("closedAt = %v, want %v", show(fields.ClosedAt), show(testCase.wantClosedAt))
			}
			if !sameTimestamp(fields.CloudOnlyAt, testCase.wantCloudOnlyAt) {
				t.Errorf("cloudOnlyAt = %v, want %v", show(fields.CloudOnlyAt), show(testCase.wantCloudOnlyAt))
			}
		})
	}
}

// TestRetainOnDeleteLeavesAnUnreadableRowAlone: the stored row is read back out of the JSON a pull
// would send, so a row this build cannot parse must fall through to an ordinary deletion rather
// than being kept on a guess.
func TestRetainOnDeleteLeavesAnUnreadableRowAlone(t *testing.T) {
	fields := NotebookFields{Name: "Field notes", ClosedAt: timestamp(1_700_000_400_000)}
	if fields.RetainOnDelete(json.RawMessage("not an object"), 1_700_000_500_000) {
		t.Fatal("an unreadable stored row was retained")
	}
}

// TestNotebookClaimsItsShelfFields: the two columns must stop being collected as unrecognised
// fields the moment they gain columns, or every notebook would carry a second stale copy of them in
// `extra` that the rendered change silently shadows.
func TestNotebookClaimsItsShelfFields(t *testing.T) {
	kind, known := SyncKindByName("notebook")
	if !known {
		t.Fatal("the notebook kind is not registered")
	}
	for _, jsonKey := range []string{"closedAt", "cloudOnlyAt"} {
		if !kind.ClaimsField(jsonKey) {
			t.Errorf("the notebook kind does not claim %q, so it would be stored in extra", jsonKey)
		}
	}
}

func sameTimestamp(actual *int64, expected *int64) bool {
	if actual == nil || expected == nil {
		return actual == expected
	}
	return *actual == *expected
}

func show(value *int64) any {
	if value == nil {
		return nil
	}
	return *value
}
